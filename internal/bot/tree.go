package bot

import (
	"bytes"
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
	cbTreeTog    = "t:tog:"
	cbTreeFile   = "t:file:"
	cbTreeHome   = "t:home"
	cbTreeRef    = "t:ref"
	cbTreeClose  = "t:close"

	maxFileSendSize = 20 * 1024 * 1024 // 20 MB max file size for telegram upload
	treeMaxLines    = 35               // max lines rendered in tree
)

// TreeStateManager maps directory paths to tokens and tracks expanded state per user.
type TreeStateManager struct {
	mu           sync.RWMutex
	rnd          *rand.Rand
	paths        map[string]string         // token -> path
	tokens       map[string]string         // path -> token
	userPaths    map[int64]string          // userID -> root path
	userMessages map[int64]int             // userID -> message ID of active tree
	expanded     map[int64]map[string]bool // userID -> set of expanded clean paths
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
		expanded:     make(map[int64]map[string]bool),
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

// ToggleExpanded toggles whether a folder is expanded in the tree for userID.
func (m *TreeStateManager) ToggleExpanded(userID int64, path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	userExp, ok := m.expanded[userID]
	if !ok {
		userExp = make(map[string]bool)
		m.expanded[userID] = userExp
	}

	clean := filepath.Clean(path)
	if userExp[clean] {
		delete(userExp, clean)
		return false
	}
	userExp[clean] = true
	return true
}

// IsExpanded returns whether a folder is currently expanded.
func (m *TreeStateManager) IsExpanded(userID int64, path string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	userExp, ok := m.expanded[userID]
	if !ok {
		return false
	}
	return userExp[filepath.Clean(path)]
}

// ClearExpanded resets all expanded folder states for userID.
func (m *TreeStateManager) ClearExpanded(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.expanded, userID)
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
	delete(m.expanded, userID)
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

	a.TreeState.ClearExpanded(user.ID)
	a.TreeState.SetUserPath(user.ID, absPath)
	richHTML, err := a.buildTreeRichMessageHTML(absPath, user.ID)
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
		a.TreeState.ClearExpanded(cq.From.ID)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Reset to Home",
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cq.From.ID)

	case data == cbTreeRef:
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Refreshed",
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cq.From.ID)

	case strings.HasPrefix(data, cbTreeTog):
		tok := strings.TrimPrefix(data, cbTreeTog)
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
		nowOpen := a.TreeState.ToggleExpanded(cq.From.ID, targetPath)
		toastText := "Opened " + filepath.Base(targetPath)
		if !nowOpen {
			toastText = "Closed " + filepath.Base(targetPath)
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            toastText,
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cq.From.ID)

	case strings.HasPrefix(data, cbTreeFile):
		tok := strings.TrimPrefix(data, cbTreeFile)
		targetPath := a.TreeState.GetPath(tok)
		if targetPath == "" {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "File reference expired. Reopen tg-tree.",
					ShowAlert:       true,
				})
				return err
			})
			return
		}

		fi, err := os.Stat(targetPath)
		if err != nil {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Cannot access file: " + err.Error(),
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		if fi.IsDir() {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Target is a directory.",
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		if fi.Size() == 0 {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Cannot send empty file (0 bytes).",
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		if fi.Size() > maxFileSendSize {
			sizeMB := float64(fi.Size()) / (1024 * 1024)
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            fmt.Sprintf("File is too large (%.1f MB > 20 MB).", sizeMB),
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		fileData, err := os.ReadFile(targetPath)
		if err != nil {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Cannot read file: " + err.Error(),
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Sending " + filepath.Base(targetPath) + "...",
			})
			return aErr
		})

		params := &bot.SendDocumentParams{
			ChatID: chatID,
		}
		sendErr := a.telegramCall(ctx, chatID, func() error {
			params.Document = &models.InputFileUpload{
				Filename: filepath.Base(targetPath),
				Data:     bytes.NewReader(fileData),
			}
			_, sErr := b.SendDocument(ctx, params)
			return sErr
		})
		if sendErr != nil {
			a.Log.Error("failed to send tree file document", "path", targetPath, "err", sendErr)
			a.showStatus(ctx, b, chatID, cq.From.ID, "Failed to send file: "+sendErr.Error())
		}
	}
}

// editTreeMessage updates the tree rich message in-place using rich_message.
func (a *App) editTreeMessage(ctx context.Context, b *bot.Bot, chatID int64, messageID int, userID int64) error {
	curPath := a.TreeState.GetUserPath(userID)
	richHTML, err := a.buildTreeRichMessageHTML(curPath, userID)
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

// buildTreeRichMessageHTML formats the tree with in-message <tg-button> folder buttons.
func (a *App) buildTreeRichMessageHTML(rootPath string, userID int64) (string, error) {
	cleanPath := filepath.Clean(rootPath)
	if _, err := os.Stat(cleanPath); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("<p>🌳 <b>Folder Explorer</b><br/>\n")
	sb.WriteString("📂 <code>" + html.EscapeString(cleanPath) + "</code></p>\n")

	sb.WriteString("<p>\n")
	lineCount := 0
	a.renderDirTree(&sb, cleanPath, "", userID, &lineCount)
	sb.WriteString("</p>\n")

	// Control bar
	sb.WriteString("<tg-button-row align=\"left\">\n")
	sb.WriteString("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:ref\">🔄 Refresh</tg-button>\n")
	sb.WriteString("  <tg-button type=\"callback_data\" style=\"danger\" data=\"t:close\">❌ Close</tg-button>\n")
	sb.WriteString("</tg-button-row>")

	return sb.String(), nil
}

// renderDirTree recursively renders directory contents, expanding only folders toggled by user.
func (a *App) renderDirTree(sb *strings.Builder, dirPath string, prefix string, userID int64, lineCount *int) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		sb.WriteString(prefix + "<i>(cannot read directory)</i><br/>\n")
		return
	}

	var dirs []os.DirEntry
	var files []os.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue // hide dot-folders and dot-files
		}
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			if e.Type()&os.ModeSymlink != 0 {
				if fi, sErr := os.Stat(filepath.Join(dirPath, e.Name())); sErr == nil && fi.IsDir() {
					dirs = append(dirs, e)
					continue
				}
			}
			files = append(files, e)
		}
	}

	sortEntries(dirs)
	sortEntries(files)

	if len(dirs) == 0 && len(files) == 0 {
		sb.WriteString(prefix + "<i>(empty)</i><br/>\n")
		return
	}

	for i, d := range dirs {
		if *lineCount >= treeMaxLines {
			sb.WriteString(prefix + "<i>... (more items truncated)</i><br/>\n")
			return
		}

		isLast := (i == len(dirs)-1 && len(files) == 0)
		branch := "├── "
		nextPrefix := prefix + "│   "
		if isLast {
			branch = "└── "
			nextPrefix = prefix + "    "
		}

		subPath := filepath.Join(dirPath, d.Name())
		tok := a.TreeState.GetToken(subPath)
		isOpen := a.TreeState.IsExpanded(userID, subPath)

		folderName := d.Name()
		if isOpen {
			folderName += "/"
		}

		sb.WriteString(fmt.Sprintf("%s%s<tg-button type=\"copy_text\" text=\"%s\"> </tg-button> <tg-button type=\"callback_data\" style=\"primary\" data=\"t:tog:%s\">%s</tg-button><br/>\n",
			prefix, branch, html.EscapeString(subPath), tok, html.EscapeString(folderName)))
		(*lineCount)++

		if isOpen {
			a.renderDirTree(sb, subPath, nextPrefix, userID, lineCount)
		}
	}

	for i, f := range files {
		if *lineCount >= treeMaxLines {
			sb.WriteString(prefix + "<i>... (more files truncated)</i><br/>\n")
			return
		}

		branch := "├── "
		if i == len(files)-1 {
			branch = "└── "
		}

		filePath := filepath.Join(dirPath, f.Name())
		tok := a.TreeState.GetToken(filePath)

		sb.WriteString(fmt.Sprintf("%s%s<tg-button type=\"copy_text\" text=\"%s\"> </tg-button> <tg-button type=\"callback_data\" data=\"t:file:%s\">%s</tg-button><br/>\n",
			prefix, branch, html.EscapeString(filePath), tok, html.EscapeString(f.Name())))
		(*lineCount)++
	}
}

func sortEntries(entries []os.DirEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
}
