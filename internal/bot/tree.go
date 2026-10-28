package bot

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	cbTreePrefix = "t:"
	cbTreeCD     = "t:cd:"
	cbTreeUp     = "t:up:"
	cbTreePage   = "t:p:"
	cbTreeHome   = "t:home"
	cbTreeRef    = "t:ref:"
	cbTreeClose  = "t:close"
	cbTreeNoop   = "t:noop"

	treePageSize = 8  // 4 rows of 2 buttons
	treeMaxLines = 25 // max lines inside <pre>
)

// TreeStateManager maps directory paths to short tokens so callback_data
// stays well below Telegram's 64-byte limit.
type TreeStateManager struct {
	mu       sync.RWMutex
	counter  uint64
	paths    map[string]string // token -> path
	tokens   map[string]string // path -> token
	homePath string
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
		paths:    make(map[string]string),
		tokens:   make(map[string]string),
		homePath: filepath.Clean(home),
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

	m.counter++
	tok := strconv.FormatUint(m.counter, 16)
	m.paths[tok] = clean
	m.tokens[clean] = tok

	// Cap memory in long-running bot instances
	if len(m.paths) > 2000 {
		m.paths = make(map[string]string)
		m.tokens = make(map[string]string)
		m.counter = 1
		tok = "1"
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

	text, markup, err := a.buildTreeMessage(absPath, 0)
	if err != nil {
		a.showStatus(ctx, b, chatID, user.ID, "Cannot open directory: "+err.Error())
		return
	}

	_ = a.telegramCall(ctx, chatID, func() error {
		_, sendErr := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        text,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: markup,
		})
		return sendErr
	})
}

// handleTreeCallback processes callback queries originating from tree buttons.
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
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: msgID,
				Text:      "🌳 <b>Folder Explorer</b> (closed)",
				ParseMode: models.ParseModeHTML,
			})
			return err
		})

	case data == cbTreeNoop:
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})

	case data == cbTreeHome:
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, a.TreeState.Home(), 0)

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
		_ = a.editTreeMessage(ctx, b, chatID, msgID, targetPath, 0)

	case strings.HasPrefix(data, cbTreeUp):
		tok := strings.TrimPrefix(data, cbTreeUp)
		cur := a.TreeState.GetPath(tok)
		if cur == "" {
			cur = a.TreeState.Home()
		}
		parent := filepath.Dir(cur)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, parent, 0)

	case strings.HasPrefix(data, cbTreeRef):
		tok := strings.TrimPrefix(data, cbTreeRef)
		cur := a.TreeState.GetPath(tok)
		if cur == "" {
			cur = a.TreeState.Home()
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Refreshed",
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cur, 0)

	case strings.HasPrefix(data, cbTreePage):
		rest := strings.TrimPrefix(data, cbTreePage)
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return
		}
		p, _ := strconv.Atoi(parts[0])
		tok := parts[1]
		cur := a.TreeState.GetPath(tok)
		if cur == "" {
			cur = a.TreeState.Home()
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return err
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cur, p)
	}
}

// editTreeMessage generates and updates the message text and inline keyboard.
func (a *App) editTreeMessage(ctx context.Context, b *bot.Bot, chatID int64, messageID int, dirPath string, page int) error {
	text, markup, err := a.buildTreeMessage(dirPath, page)
	if err != nil {
		return err
	}
	return a.telegramCall(ctx, chatID, func() error {
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   messageID,
			Text:        text,
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: markup,
		})
		if editErr != nil && strings.Contains(editErr.Error(), "message is not modified") {
			return nil
		}
		return editErr
	})
}

// buildTreeMessage scans dirPath and produces the HTML text and inline keyboard.
func (a *App) buildTreeMessage(dirPath string, page int) (string, *models.InlineKeyboardMarkup, error) {
	cleanPath := filepath.Clean(dirPath)
	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return "", nil, err
	}

	var dirs []os.DirEntry
	var files []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			// Check if symlink to directory
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

	// Build tree text preview
	var treeLines []string
	treeLines = append(treeLines, filepath.Base(cleanPath)+"/")

	allEntries := append([]os.DirEntry{}, dirs...)
	allEntries = append(allEntries, files...)

	lineCount := 0
	for i, entry := range allEntries {
		if lineCount >= treeMaxLines {
			treeLines = append(treeLines, fmt.Sprintf("... (%d more items)", len(allEntries)-i))
			break
		}

		isLast := i == len(allEntries)-1
		prefix := "├── "
		if isLast {
			prefix = "└── "
		}

		if entry.IsDir() {
			treeLines = append(treeLines, prefix+"📁 "+entry.Name())
			lineCount++

			// Peek sub-items for 1 level deeper
			subPath := filepath.Join(cleanPath, entry.Name())
			if subEntries, sErr := os.ReadDir(subPath); sErr == nil && len(subEntries) > 0 {
				subDirs := []os.DirEntry{}
				for _, se := range subEntries {
					if se.IsDir() {
						subDirs = append(subDirs, se)
					}
				}
				sortEntries(subDirs)
				subPrefix := "│   "
				if isLast {
					subPrefix = "    "
				}
				limit := 3
				if len(subDirs) < limit {
					limit = len(subDirs)
				}
				for j := 0; j < limit && lineCount < treeMaxLines; j++ {
					subIsLast := j == len(subDirs)-1 && len(subDirs) <= 3
					subItemPrefix := "├── "
					if subIsLast {
						subItemPrefix = "└── "
					}
					treeLines = append(treeLines, subPrefix+subItemPrefix+"📁 "+subDirs[j].Name())
					lineCount++
				}
				if len(subDirs) > limit && lineCount < treeMaxLines {
					treeLines = append(treeLines, subPrefix+"└── ... ("+strconv.Itoa(len(subDirs)-limit)+" more)")
					lineCount++
				}
			}
		} else {
			treeLines = append(treeLines, prefix+"📄 "+entry.Name())
			lineCount++
		}
	}

	if len(allEntries) == 0 {
		treeLines = append(treeLines, "  (empty directory)")
	}

	treeContent := strings.Join(treeLines, "\n")

	var sb strings.Builder
	sb.WriteString("🌳 <b>Folder Explorer</b>\n")
	sb.WriteString("📂 <code>" + html.EscapeString(cleanPath) + "</code>\n\n")
	sb.WriteString("<pre>" + html.EscapeString(treeContent) + "</pre>\n")
	sb.WriteString(fmt.Sprintf("📊 <i>%d directories, %d files</i>", len(dirs), len(files)))

	curToken := a.TreeState.GetToken(cleanPath)

	// Build inline buttons for subdirectories
	totalPages := (len(dirs) + treePageSize - 1) / treePageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	var rows [][]models.InlineKeyboardButton

	start := page * treePageSize
	end := start + treePageSize
	if end > len(dirs) {
		end = len(dirs)
	}

	var curRow []models.InlineKeyboardButton
	for _, d := range dirs[start:end] {
		subDir := filepath.Join(cleanPath, d.Name())
		tok := a.TreeState.GetToken(subDir)
		btn := models.InlineKeyboardButton{
			Text:         "📁 " + truncateBtnText(d.Name(), 20),
			CallbackData: cbTreeCD + tok,
		}
		curRow = append(curRow, btn)
		if len(curRow) == 2 {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 {
		rows = append(rows, curRow)
	}

	// Pagination row
	if totalPages > 1 {
		var pageRow []models.InlineKeyboardButton
		if page > 0 {
			pageRow = append(pageRow, models.InlineKeyboardButton{
				Text:         "◀️ Prev",
				CallbackData: fmt.Sprintf("%s%d:%s", cbTreePage, page-1, curToken),
			})
		}
		pageRow = append(pageRow, models.InlineKeyboardButton{
			Text:         fmt.Sprintf("%d/%d", page+1, totalPages),
			CallbackData: cbTreeNoop,
		})
		if page < totalPages-1 {
			pageRow = append(pageRow, models.InlineKeyboardButton{
				Text:         "Next ▶️",
				CallbackData: fmt.Sprintf("%s%d:%s", cbTreePage, page+1, curToken),
			})
		}
		rows = append(rows, pageRow)
	}

	// Bottom navigation row
	var ctrlRow []models.InlineKeyboardButton
	if cleanPath != "/" {
		ctrlRow = append(ctrlRow, models.InlineKeyboardButton{
			Text:         "⬆️ Up",
			CallbackData: cbTreeUp + curToken,
		})
	}
	ctrlRow = append(ctrlRow, models.InlineKeyboardButton{
		Text:         "🏠 Home",
		CallbackData: cbTreeHome,
	})
	ctrlRow = append(ctrlRow, models.InlineKeyboardButton{
		Text:         "🔄 Refresh",
		CallbackData: cbTreeRef + curToken,
	})
	ctrlRow = append(ctrlRow, models.InlineKeyboardButton{
		Text:         "❌ Close",
		CallbackData: cbTreeClose,
	})
	rows = append(rows, ctrlRow)

	markup := &models.InlineKeyboardMarkup{
		InlineKeyboard: rows,
	}

	return sb.String(), markup, nil
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

func truncateBtnText(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes-2]) + ".."
}
