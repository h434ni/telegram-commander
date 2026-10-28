package bot

import (
	"context"
	"fmt"
	"html"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	cbTreePrefix = "t:"
	cbTreeCD     = "t:cd:"
	cbTreeUp     = "t:up:"
	cbTreeHome   = "t:home"
	cbTreeRef    = "t:ref:"
	cbTreeClose  = "t:close"

	treeMaxLines = 30 // max lines of directory entries
)

// TreeStateManager maps directory paths to short tokens so callback_data
// stays well below Telegram's 64-byte limit.
type TreeStateManager struct {
	mu           sync.RWMutex
	rnd          *rand.Rand
	paths        map[string]string // token -> path
	tokens       map[string]string // path -> token
	userPaths    map[int64]string  // userID -> current path
	userMessages map[int64]int     // userID -> message ID of active tree
	homePath     string
}

// NewTreeStateManager creates a new state manager with a default home path.
func NewTreeStateManager(home string) *TreeStateManager {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		home = "/home/ec2-user"
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	return &TreeStateManager{
		rnd:          rand.New(rand.NewSource(time.Now().UnixNano())),
		paths:        make(map[string]string),
		tokens:       make(map[string]string),
		userPaths:    make(map[int64]string),
		userMessages: make(map[int64]int),
		homePath:     filepath.Clean(home),
	}
}

// Home returns the configured home directory.
func (m *TreeStateManager) Home() string {
	return m.homePath
}

// GetToken returns an existing or newly allocated token for path.
func (m *TreeStateManager) GetToken(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	clean := filepath.Clean(path)
	if tok, ok := m.tokens[clean]; ok {
		return tok
	}

	var tok string
	for {
		tok = fmt.Sprintf("%04x", m.rnd.Intn(0x10000))
		if _, exists := m.paths[tok]; !exists {
			break
		}
	}

	m.paths[tok] = clean
	m.tokens[clean] = tok

	if len(m.paths) > 2000 {
		m.paths = make(map[string]string)
		m.tokens = make(map[string]string)
		tok = fmt.Sprintf("%04x", m.rnd.Intn(0x10000))
		m.paths[tok] = clean
		m.tokens[clean] = tok
	}

	return tok
}

// GetPath looks up the directory path for a token.
func (m *TreeStateManager) GetPath(token string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.paths[token]
}

// SetUserPath records the directory the user is currently browsing.
func (m *TreeStateManager) SetUserPath(userID int64, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userPaths[userID] = filepath.Clean(path)
}

// GetUserPath returns the directory the user is currently browsing, or Home.
func (m *TreeStateManager) GetUserPath(userID int64) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.userPaths[userID]; ok && p != "" {
		return p
	}
	return m.homePath
}

// SetUserMessage records the message ID of the active tree message.
func (m *TreeStateManager) SetUserMessage(userID int64, msgID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userMessages[userID] = msgID
}

// GetUserMessageID returns the last tree message ID for this user.
func (m *TreeStateManager) GetUserMessageID(userID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.userMessages[userID]
}

// ClearUserMessage resets the tracked tree message for the user.
func (m *TreeStateManager) ClearUserMessage(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.userMessages, userID)
}

// isTreeButton returns true if the node targets the tree feature.
func (a *App) isTreeButton(node *Node) bool {
	if node == nil {
		return false
	}
	if strings.EqualFold(node.Function, "tg-tree") || strings.EqualFold(node.Function, "tree") {
		return true
	}
	if strings.EqualFold(node.Name, "tg-tree") || strings.EqualFold(node.ID, "tg-tree") {
		return true
	}
	return false
}

// startTreeExplorer initiates the tree explorer message using rich text format with in-message buttons.
func (a *App) startTreeExplorer(ctx context.Context, b *bot.Bot, chatID int64, user *models.User, node *Node) {
	startPath := ""
	if node.Path != "" {
		startPath = node.Path
	} else if p, ok := node.Raw.Params["path"]; ok && p != "" {
		startPath = p
	} else if node.WorkDir != "" {
		startPath = node.WorkDir
	} else if a.Cfg.WorkDir != "" {
		startPath = a.Cfg.WorkDir
	} else {
		startPath = a.TreeState.Home()
	}

	absPath, err := filepath.Abs(startPath)
	if err != nil {
		a.showStatus(ctx, b, chatID, user.ID, "Invalid path: "+err.Error())
		return
	}

	a.TreeState.SetUserPath(user.ID, absPath)
	richHTML, err := a.buildTreeRichMessageHTML(absPath)
	if err != nil {
		a.showStatus(ctx, b, chatID, user.ID, "Cannot open directory: "+err.Error())
		return
	}

	_ = a.telegramCall(ctx, chatID, func() error {
		msg, sendErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
			ChatID: chatID,
			RichMessage: models.InputRichMessage{
				HTML: richHTML,
			},
		})
		if sendErr == nil && msg != nil {
			a.TreeState.SetUserMessage(user.ID, msg.ID)
		}
		return sendErr
	})
}

// handleTreeCallback handles callback queries originating from in-message <tg-button> elements.
func (a *App) handleTreeCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	data := cq.Data
	chatID := int64(0)
	msgID := 0
	if cq.Message.Message != nil {
		chatID = cq.Message.Message.Chat.ID
		msgID = cq.Message.Message.ID
	}
	if chatID == 0 || msgID == 0 {
		return
	}

	switch {
	case data == cbTreeClose:
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Explorer closed",
			})
			return err
		})
		a.closeTreeExplorer(ctx, b, chatID, cq.From.ID)

	case data == cbTreeHome:
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, a.TreeState.Home())

	case strings.HasPrefix(data, cbTreeCD):
		tok := strings.TrimPrefix(data, cbTreeCD)
		targetPath := a.TreeState.GetPath(tok)
		if targetPath == "" {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Directory expired. Reopen tg-tree.",
					ShowAlert:       true,
				})
				return err
			})
			return
		}
		if _, err := os.ReadDir(targetPath); err != nil {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Cannot access directory: " + err.Error(),
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		a.TreeState.SetUserPath(cq.From.ID, targetPath)
		_ = a.editTreeMessage(ctx, b, chatID, msgID, targetPath)

	case strings.HasPrefix(data, cbTreeUp):
		tok := strings.TrimPrefix(data, cbTreeUp)
		cur := a.TreeState.GetPath(tok)
		if cur == "" {
			cur = a.TreeState.GetUserPath(cq.From.ID)
		}
		parent := filepath.Dir(cur)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		a.TreeState.SetUserPath(cq.From.ID, parent)
		_ = a.editTreeMessage(ctx, b, chatID, msgID, parent)

	case strings.HasPrefix(data, cbTreeRef):
		tok := strings.TrimPrefix(data, cbTreeRef)
		cur := a.TreeState.GetPath(tok)
		if cur == "" {
			cur = a.TreeState.GetUserPath(cq.From.ID)
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Refreshed",
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cur)
	}
}

// handleTreeCommand allows optional text fallback commands like /cd_home or /cd_close.
func (a *App) handleTreeCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)

	_ = a.telegramCall(ctx, chatID, func() error {
		_, err := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    chatID,
			MessageID: msg.ID,
		})
		return err
	})

	switch {
	case text == "/cd_close":
		a.closeTreeExplorer(ctx, b, chatID, userID)
	case text == "/cd_home":
		a.TreeState.SetUserPath(userID, a.TreeState.Home())
		treeMsgID := a.TreeState.GetUserMessageID(userID)
		_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, a.TreeState.Home())
	case text == "/cd_up":
		cur := a.TreeState.GetUserPath(userID)
		parent := filepath.Dir(cur)
		a.TreeState.SetUserPath(userID, parent)
		treeMsgID := a.TreeState.GetUserMessageID(userID)
		_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, parent)
	case text == "/cd_ref":
		cur := a.TreeState.GetUserPath(userID)
		treeMsgID := a.TreeState.GetUserMessageID(userID)
		_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, cur)
	case strings.HasPrefix(text, "/cd_"):
		tok := strings.TrimPrefix(text, "/cd_")
		targetPath := a.TreeState.GetPath(tok)
		if targetPath == "" {
			a.showStatus(ctx, b, chatID, userID, "Unknown or expired directory command.")
			return
		}
		a.TreeState.SetUserPath(userID, targetPath)
		treeMsgID := a.TreeState.GetUserMessageID(userID)
		_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, targetPath)
	}
}

// editTreeMessage updates the tree rich message in-place using rich_message.
func (a *App) editTreeMessage(ctx context.Context, b *bot.Bot, chatID int64, messageID int, dirPath string) error {
	richHTML, err := a.buildTreeRichMessageHTML(dirPath)
	if err != nil {
		return err
	}
	return a.telegramCall(ctx, chatID, func() error {
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: messageID,
			RichMessage: &models.InputRichMessage{
				HTML: richHTML,
			},
		})
		if editErr != nil && strings.Contains(editErr.Error(), "message is not modified") {
			return nil
		}
		return editErr
	})
}

// closeTreeExplorer updates the message to closed.
func (a *App) closeTreeExplorer(ctx context.Context, b *bot.Bot, chatID, userID int64) {
	treeMsgID := a.TreeState.GetUserMessageID(userID)
	if treeMsgID != 0 {
		cur := a.TreeState.GetUserPath(userID)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: treeMsgID,
				RichMessage: &models.InputRichMessage{
					HTML: fmt.Sprintf("<p>🌳 <b>Folder Explorer</b> (closed)<br/><i>Last path:</i> <code>%s</code></p>", html.EscapeString(cur)),
				},
			})
			return err
		})
		a.TreeState.ClearUserMessage(userID)
	}
}

// buildTreeRichMessageHTML formats the tree with in-message <tg-button> and <tg-button-row> rich text tags.
func (a *App) buildTreeRichMessageHTML(dirPath string) (string, error) {
	cleanPath := filepath.Clean(dirPath)
	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return "", err
	}

	var dirs []os.DirEntry
	var files []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			if e.Type()&os.ModeSymlink != 0 {
				if fi, sErr := os.Stat(filepath.Join(cleanPath, e.Name())); sErr == nil && fi.IsDir() {
					dirs = append(dirs, e)
					continue
				}
			}
			files = append(files, e)
		}
	}

	sortEntries(dirs)
	sortEntries(files)

	var sb strings.Builder
	sb.WriteString("<p>🌳 <b>Folder Explorer</b><br/>\n")
	sb.WriteString("📂 <code>" + html.EscapeString(cleanPath) + "</code></p>\n")

	lineCount := 0

	if len(dirs) > 0 || len(files) > 0 {
		sb.WriteString("<p>")
		for _, d := range dirs {
			if lineCount >= treeMaxLines {
				sb.WriteString(fmt.Sprintf("<i>... and %d more directories</i><br/>\n", len(dirs)-lineCount))
				break
			}

			subDirPath := filepath.Join(cleanPath, d.Name())
			tok := a.TreeState.GetToken(subDirPath)
			sb.WriteString(fmt.Sprintf("├── 📁 <b>%s</b> <tg-button type=\"callback_data\" style=\"primary\" data=\"t:cd:%s\">Open</tg-button><br/>\n", html.EscapeString(d.Name()), tok))
			lineCount++

			// Peek sub-items for visual hierarchy
			if subEntries, sErr := os.ReadDir(subDirPath); sErr == nil && len(subEntries) > 0 {
				var subDirs []os.DirEntry
				for _, se := range subEntries {
					if se.IsDir() {
						subDirs = append(subDirs, se)
					}
				}
				sortEntries(subDirs)
				limit := 3
				if len(subDirs) < limit {
					limit = len(subDirs)
				}
				for j := 0; j < limit && lineCount < treeMaxLines; j++ {
					subSubPath := filepath.Join(subDirPath, subDirs[j].Name())
					subTok := a.TreeState.GetToken(subSubPath)
					subPrefix := "│   ├─ "
					if j == limit-1 && len(subDirs) <= limit {
						subPrefix = "│   └─ "
					}
					sb.WriteString(fmt.Sprintf("%s📁 %s <tg-button type=\"callback_data\" style=\"link\" data=\"t:cd:%s\">Open</tg-button><br/>\n", subPrefix, html.EscapeString(subDirs[j].Name()), subTok))
					lineCount++
				}
				if len(subDirs) > limit && lineCount < treeMaxLines {
					sb.WriteString(fmt.Sprintf("│   └─ <i>... +%d more</i><br/>\n", len(subDirs)-limit))
					lineCount++
				}
			}
		}

		fileLimit := 10
		if len(files) < fileLimit {
			fileLimit = len(files)
		}
		for i := 0; i < fileLimit && lineCount < treeMaxLines; i++ {
			prefix := "├── "
			if i == len(files)-1 && len(files) <= fileLimit {
				prefix = "└── "
			}
			sb.WriteString(fmt.Sprintf("%s📄 %s<br/>\n", prefix, html.EscapeString(files[i].Name())))
			lineCount++
		}
		if len(files) > fileLimit {
			sb.WriteString(fmt.Sprintf("└── <i>... and %d more files</i><br/>\n", len(files)-fileLimit))
		}
		sb.WriteString("</p>\n")
	} else {
		sb.WriteString("<p><i>(empty directory)</i></p>\n")
	}

	sb.WriteString(fmt.Sprintf("<p>📊 <i>%d directories, %d files</i></p>\n", len(dirs), len(files)))

	// Rich text in-message button row
	curToken := a.TreeState.GetToken(cleanPath)
	sb.WriteString("<tg-button-row align=\"left\">\n")
	if cleanPath != "/" {
		sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"primary\" data=\"t:up:%s\">⬆️ Up</tg-button>\n", curToken))
	}
	sb.WriteString("  <tg-button type=\"callback_data\" style=\"primary\" data=\"t:home\">🏠 Home</tg-button>\n")
	sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:ref:%s\">🔄 Refresh</tg-button>\n", curToken))
	sb.WriteString("  <tg-button type=\"callback_data\" style=\"danger\" data=\"t:close\">❌ Close</tg-button>\n")
	sb.WriteString("</tg-button-row>")

	return sb.String(), nil
}

func sortEntries(entries []os.DirEntry) {
	sort.Slice(entries, func(i, j int) bool {
		nameA := entries[i].Name()
		nameB := entries[j].Name()
		hiddenA := strings.HasPrefix(nameA, ".")
		hiddenB := strings.HasPrefix(nameB, ".")
		if hiddenA != hiddenB {
			return !hiddenA // non-hidden first
		}
		return strings.ToLower(nameA) < strings.ToLower(nameB)
	})
}
