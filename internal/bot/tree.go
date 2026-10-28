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
	treeMaxLines = 30 // max lines of directory entries
)

// TreeStateManager maps directory paths to short random hex tokens so commands
// like /cd_a1b2 can be tapped in rich text directly inside the message.
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

// GetToken returns an existing or newly allocated random token for path.
func (m *TreeStateManager) GetToken(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	clean := filepath.Clean(path)
	if tok, ok := m.tokens[clean]; ok {
		return tok
	}

	// Generate a unique 4-character random hex token
	var tok string
	for {
		tok = fmt.Sprintf("%04x", m.rnd.Intn(0x10000))
		if _, exists := m.paths[tok]; !exists {
			break
		}
	}

	m.paths[tok] = clean
	m.tokens[clean] = tok

	// Cap memory in long-running bot instances
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

// startTreeExplorer initiates the tree explorer message.
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
	htmlText, err := a.buildTreeRichText(absPath)
	if err != nil {
		a.showStatus(ctx, b, chatID, user.ID, "Cannot open directory: "+err.Error())
		return
	}

	_ = a.telegramCall(ctx, chatID, func() error {
		msg, sendErr := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      htmlText,
			ParseMode: models.ParseModeHTML,
		})
		if sendErr == nil && msg != nil {
			a.TreeState.SetUserMessage(user.ID, msg.ID)
		}
		return sendErr
	})
}

// handleTreeCommand routes in-message rich text slash commands like /cd_a1b2.
func (a *App) handleTreeCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)

	// Clean up the user's command message so the chat isn't cluttered
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
		a.navigateTree(ctx, b, chatID, userID, a.TreeState.Home())
	case text == "/cd_up":
		cur := a.TreeState.GetUserPath(userID)
		parent := filepath.Dir(cur)
		a.navigateTree(ctx, b, chatID, userID, parent)
	case text == "/cd_ref":
		cur := a.TreeState.GetUserPath(userID)
		a.navigateTree(ctx, b, chatID, userID, cur)
	case strings.HasPrefix(text, "/cd_"):
		tok := strings.TrimPrefix(text, "/cd_")
		targetPath := a.TreeState.GetPath(tok)
		if targetPath == "" {
			a.showStatus(ctx, b, chatID, userID, "Unknown or expired directory command: "+text)
			return
		}
		a.navigateTree(ctx, b, chatID, userID, targetPath)
	}
}

// navigateTree updates the tree message to targetPath in-place.
func (a *App) navigateTree(ctx context.Context, b *bot.Bot, chatID, userID int64, targetPath string) {
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		a.showStatus(ctx, b, chatID, userID, "Invalid path: "+err.Error())
		return
	}

	if _, err := os.ReadDir(absPath); err != nil {
		a.showStatus(ctx, b, chatID, userID, "Cannot access "+absPath+": "+err.Error())
		return
	}

	a.TreeState.SetUserPath(userID, absPath)
	htmlText, err := a.buildTreeRichText(absPath)
	if err != nil {
		a.showStatus(ctx, b, chatID, userID, "Cannot read directory: "+err.Error())
		return
	}

	treeMsgID := a.TreeState.GetUserMessageID(userID)
	if treeMsgID != 0 {
		var editErr error
		_ = a.telegramCall(ctx, chatID, func() error {
			_, editErr = b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: treeMsgID,
				Text:      htmlText,
				ParseMode: models.ParseModeHTML,
			})
			if editErr != nil && strings.Contains(editErr.Error(), "message is not modified") {
				return nil
			}
			return editErr
		})
		if editErr == nil || (editErr != nil && strings.Contains(editErr.Error(), "message is not modified")) {
			return
		}
	}

	// Fallback to sending a new message if edit fails (e.g. previous message was deleted)
	_ = a.telegramCall(ctx, chatID, func() error {
		msg, sendErr := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      htmlText,
			ParseMode: models.ParseModeHTML,
		})
		if sendErr == nil && msg != nil {
			a.TreeState.SetUserMessage(userID, msg.ID)
		}
		return sendErr
	})
}

// closeTreeExplorer marks the tree message as closed.
func (a *App) closeTreeExplorer(ctx context.Context, b *bot.Bot, chatID, userID int64) {
	treeMsgID := a.TreeState.GetUserMessageID(userID)
	if treeMsgID != 0 {
		cur := a.TreeState.GetUserPath(userID)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: treeMsgID,
				Text:      fmt.Sprintf("🌳 <b>Folder Explorer</b> (closed)\n<i>Last path:</i> <code>%s</code>", html.EscapeString(cur)),
				ParseMode: models.ParseModeHTML,
			})
			return err
		})
		a.TreeState.ClearUserMessage(userID)
	}
}

// buildTreeRichText formats the directory tree using rich text with /cd_<token> command buttons.
func (a *App) buildTreeRichText(dirPath string) (string, error) {
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
	sb.WriteString("🌳 <b>Folder Explorer</b>\n")
	sb.WriteString("📂 <code>" + html.EscapeString(cleanPath) + "</code>\n\n")

	lineCount := 0

	if len(dirs) > 0 {
		sb.WriteString("📁 <b>Directories:</b>\n")
		for _, d := range dirs {
			if lineCount >= treeMaxLines {
				sb.WriteString(fmt.Sprintf("  <i>... and %d more directories</i>\n", len(dirs)-lineCount))
				break
			}

			subDirPath := filepath.Join(cleanPath, d.Name())
			tok := a.TreeState.GetToken(subDirPath)
			sb.WriteString(fmt.Sprintf("• 📁 <b>%s</b> — /cd_%s\n", html.EscapeString(d.Name()), tok))
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
					subPrefix := "  ├─ "
					if j == limit-1 && len(subDirs) <= limit {
						subPrefix = "  └─ "
					}
					sb.WriteString(fmt.Sprintf("%s📁 %s — /cd_%s\n", subPrefix, html.EscapeString(subDirs[j].Name()), subTok))
					lineCount++
				}
				if len(subDirs) > limit && lineCount < treeMaxLines {
					sb.WriteString(fmt.Sprintf("  └─ <i>... +%d more</i>\n", len(subDirs)-limit))
					lineCount++
				}
			}
		}
		sb.WriteString("\n")
	}

	if len(files) > 0 {
		sb.WriteString("📄 <b>Files:</b>\n")
		fileLimit := 10
		if len(files) < fileLimit {
			fileLimit = len(files)
		}
		for i := 0; i < fileLimit && lineCount < treeMaxLines; i++ {
			sb.WriteString(fmt.Sprintf("• 📄 %s\n", html.EscapeString(files[i].Name())))
			lineCount++
		}
		if len(files) > fileLimit {
			sb.WriteString(fmt.Sprintf("• <i>... and %d more files</i>\n", len(files)-fileLimit))
		}
		sb.WriteString("\n")
	}

	if len(dirs) == 0 && len(files) == 0 {
		sb.WriteString("<i>(empty directory)</i>\n\n")
	}

	sb.WriteString(fmt.Sprintf("📊 <i>%d directories, %d files</i>\n\n", len(dirs), len(files)))

	// Rich text command navigation bar
	sb.WriteString("<b>Navigation:</b>\n")
	var navItems []string
	if cleanPath != "/" {
		navItems = append(navItems, "⬆️ <b>Up:</b> /cd_up")
	}
	navItems = append(navItems, "🏠 <b>Home:</b> /cd_home")
	navItems = append(navItems, "🔄 <b>Refresh:</b> /cd_ref")
	navItems = append(navItems, "❌ <b>Close:</b> /cd_close")
	sb.WriteString(strings.Join(navItems, "  •  "))

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
