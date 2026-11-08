package bot

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/http"
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
	cbTreeSend   = "t:send:"
	cbTreeZip    = "t:zip:"
	cbTreeEdit   = "t:edit:"
	cbTreeRen    = "t:ren:"
	cbTreeRfeY   = "t:rfe:y:"
	cbTreeRfeN   = "t:rfe:n:"
	cbTreeRfdY   = "t:rfd:y:"
	cbTreeRfdN   = "t:rfd:n:"
	cbTreeHome   = "t:home"
	cbTreeRef    = "t:ref"
	cbTreeClose  = "t:close"

	maxFileSendSize = 20 * 1024 * 1024 // 20 MB max file size for telegram upload
	treeMaxLines    = 35               // max lines rendered in tree
)

// TreeActionType represents a pending user action in tree explorer.
type TreeActionType int

const (
	TreeActionNone TreeActionType = iota
	TreeActionEdit
	TreeActionRename
)

// TreeAwaiting tracks user awaiting state for file edit or rename.
type TreeAwaiting struct {
	Action      TreeActionType
	TargetPath  string
	PromptMsgID int
}

// TreeStateManager maps directory paths to tokens and tracks expanded state per user.
type TreeStateManager struct {
	mu            sync.RWMutex
	rnd           *rand.Rand
	paths         map[string]string         // token -> path
	tokens        map[string]string         // path -> token
	userPaths     map[int64]string          // userID -> root path
	userMessages  map[int64]int             // userID -> message ID of active tree
	expanded      map[int64]map[string]bool // userID -> set of expanded clean paths
	selectedItems map[int64]string          // userID -> last clicked folder or file path
	awaiting      map[int64]TreeAwaiting    // userID -> pending edit/rename action
	homePath      string
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
		rnd:           rand.New(rand.NewSource(time.Now().UnixNano())),
		paths:         make(map[string]string),
		tokens:        make(map[string]string),
		userPaths:     make(map[int64]string),
		userMessages:  make(map[int64]int),
		expanded:      make(map[int64]map[string]bool),
		selectedItems: make(map[int64]string),
		awaiting:      make(map[int64]TreeAwaiting),
		homePath:      filepath.Clean(home),
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

// SetSelectedItem records the folder or file last clicked by the user.
func (m *TreeStateManager) SetSelectedItem(userID int64, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if path == "" {
		delete(m.selectedItems, userID)
		return
	}
	m.selectedItems[userID] = filepath.Clean(path)
}

// GetSelectedItem returns the folder or file last clicked by the user.
func (m *TreeStateManager) GetSelectedItem(userID int64) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selectedItems[userID]
}

// SetSelectedFolder records the folder last clicked by the user (alias for SetSelectedItem).
func (m *TreeStateManager) SetSelectedFolder(userID int64, path string) {
	m.SetSelectedItem(userID, path)
}

// GetSelectedFolder returns the folder last clicked by the user (alias for GetSelectedItem).
func (m *TreeStateManager) GetSelectedFolder(userID int64) string {
	return m.GetSelectedItem(userID)
}

// ClearUserMessage resets the tracked tree message for the user.
func (m *TreeStateManager) ClearUserMessage(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.userMessages, userID)
	delete(m.expanded, userID)
	delete(m.selectedItems, userID)
	delete(m.awaiting, userID)
}

// SetAwaiting records pending action awaiting user input.
func (m *TreeStateManager) SetAwaiting(userID int64, action TreeActionType, path string, promptMsgID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if action == TreeActionNone {
		delete(m.awaiting, userID)
		return
	}
	m.awaiting[userID] = TreeAwaiting{
		Action:      action,
		TargetPath:  filepath.Clean(path),
		PromptMsgID: promptMsgID,
	}
}

// GetAwaiting retrieves any pending tree awaiting action for userID.
func (m *TreeStateManager) GetAwaiting(userID int64) (TreeAwaiting, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	aw, ok := m.awaiting[userID]
	return aw, ok
}

// ClearAwaiting clears pending tree awaiting state for userID.
func (m *TreeStateManager) ClearAwaiting(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.awaiting, userID)
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
	a.TreeState.SetSelectedFolder(user.ID, "")
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
		a.TreeState.SetSelectedFolder(cq.From.ID, targetPath)
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

		a.TreeState.SetSelectedItem(cq.From.ID, targetPath)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Selected " + filepath.Base(targetPath),
			})
			return aErr
		})
		_ = a.editTreeMessage(ctx, b, chatID, msgID, cq.From.ID)

	case strings.HasPrefix(data, cbTreeZip):
		tok := strings.TrimPrefix(data, cbTreeZip)
		targetPath := a.TreeState.GetPath(tok)
		if targetPath == "" {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Directory reference expired. Reopen tg-tree.",
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
					Text:            "Cannot access directory: " + err.Error(),
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		if !fi.IsDir() {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
					CallbackQueryID: cq.ID,
					Text:            "Target is not a directory.",
					ShowAlert:       true,
				})
				return aErr
			})
			return
		}

		ansErr := a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return aErr
		})
		if ansErr != nil {
			a.Log.Error("AnswerCallbackQuery error in cbTreeZip", "err", ansErr, "cq_id", cq.ID)
		} else {
			a.Log.Info("AnswerCallbackQuery success in cbTreeZip", "cq_id", cq.ID)
		}

		zipData, err := createZipArchive(targetPath, maxFileSendSize)
		if err != nil {
			a.Log.Error("failed to create zip archive", "path", targetPath, "err", err)
			alertText := "Failed to zip directory: " + err.Error()
			if strings.Contains(err.Error(), "exceeds maximum size") {
				alertText = fmt.Sprintf("Folder is too large (> %d MB).", maxFileSendSize/(1024*1024))
			}
			a.showStatus(ctx, b, chatID, cq.From.ID, alertText)
			return
		}

		zipFilename := filepath.Base(targetPath) + ".zip"
		params := &bot.SendDocumentParams{
			ChatID: chatID,
		}
		sendErr := a.telegramCall(ctx, chatID, func() error {
			params.Document = &models.InputFileUpload{
				Filename: zipFilename,
				Data:     bytes.NewReader(zipData),
			}
			_, sErr := b.SendDocument(ctx, params)
			return sErr
		})
		if sendErr != nil {
			a.Log.Error("failed to send folder zip document", "path", targetPath, "err", sendErr)
			a.showStatus(ctx, b, chatID, cq.From.ID, "Failed to send zip: "+sendErr.Error())
		}

	case strings.HasPrefix(data, cbTreeSend):
		tok := strings.TrimPrefix(data, cbTreeSend)
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

		ansErr := a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return aErr
		})
		if ansErr != nil {
			a.Log.Error("AnswerCallbackQuery error in cbTreeSend", "err", ansErr, "cq_id", cq.ID)
		} else {
			a.Log.Info("AnswerCallbackQuery success in cbTreeSend", "cq_id", cq.ID)
		}

		fileData, err := os.ReadFile(targetPath)
		if err != nil {
			a.Log.Error("failed to read file", "path", targetPath, "err", err)
			a.showStatus(ctx, b, chatID, cq.From.ID, "Cannot read file: "+err.Error())
			return
		}

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

	case strings.HasPrefix(data, cbTreeEdit):
		tok := strings.TrimPrefix(data, cbTreeEdit)
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

		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return aErr
		})

		ext := filepath.Ext(targetPath)
		promptText := fmt.Sprintf("Send text or a file with the same format (%s) to replace '%s', or /cancel to abort.", ext, filepath.Base(targetPath))
		if ext == "" {
			promptText = fmt.Sprintf("Send text or a file without format to replace '%s', or /cancel to abort.", filepath.Base(targetPath))
		}
		var promptMsg *models.Message
		_ = a.telegramCall(ctx, chatID, func() error {
			var sendErr error
			promptMsg, sendErr = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      chatID,
				Text:        promptText,
				ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
			})
			return sendErr
		})
		promptID := 0
		if promptMsg != nil {
			promptID = promptMsg.ID
		}
		a.TreeState.SetAwaiting(cq.From.ID, TreeActionEdit, targetPath, promptID)

	case strings.HasPrefix(data, cbTreeRen):
		tok := strings.TrimPrefix(data, cbTreeRen)
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

		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
			})
			return aErr
		})

		var promptMsg *models.Message
		_ = a.telegramCall(ctx, chatID, func() error {
			var sendErr error
			promptMsg, sendErr = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      chatID,
				Text:        fmt.Sprintf("Send the new name for '%s', or /cancel to abort.", filepath.Base(targetPath)),
				ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
			})
			return sendErr
		})
		promptID := 0
		if promptMsg != nil {
			promptID = promptMsg.ID
		}
		a.TreeState.SetAwaiting(cq.From.ID, TreeActionRename, targetPath, promptID)

	case strings.HasPrefix(data, cbTreeRfeY):
		parts := strings.Split(strings.TrimPrefix(data, cbTreeRfeY), ":")
		if len(parts) == 2 {
			oldPath := a.TreeState.GetPath(parts[0])
			newPath := a.TreeState.GetPath(parts[1])
			if oldPath != "" && newPath != "" {
				err := os.Rename(oldPath, newPath)
				if err != nil {
					_ = a.telegramCall(ctx, chatID, func() error {
						_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
							CallbackQueryID: cq.ID,
							Text:            "Failed to rename: " + err.Error(),
							ShowAlert:       true,
						})
						return aErr
					})
					return
				}
				a.TreeState.SetSelectedItem(cq.From.ID, newPath)
				_ = a.telegramCall(ctx, chatID, func() error {
					_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
						CallbackQueryID: cq.ID,
						Text:            "Renamed to " + filepath.Base(newPath),
					})
					return aErr
				})
				if treeMsgID := a.TreeState.GetUserMessageID(cq.From.ID); treeMsgID != 0 {
					_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, cq.From.ID)
				}
				_ = a.telegramCall(ctx, chatID, func() error {
					_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: msgID,
						RichMessage: &models.InputRichMessage{
							HTML: fmt.Sprintf("<p>File renamed to <code>%s</code>.</p>", html.EscapeString(filepath.Base(newPath))),
						},
					})
					if editErr != nil && strings.Contains(editErr.Error(), "message can't be edited") {
						_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
							ChatID:    chatID,
							MessageID: msgID,
						})
						_, _ = b.SendRichMessage(ctx, &bot.SendRichMessageParams{
							ChatID: chatID,
							RichMessage: models.InputRichMessage{
								HTML: fmt.Sprintf("<p>File renamed to <code>%s</code>.</p>", html.EscapeString(filepath.Base(newPath))),
							},
							ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
						})
						return nil
					}
					return editErr
				})
				_ = a.sendUpdatedTreeMessage(ctx, b, chatID, cq.From.ID)
			}
		}

	case strings.HasPrefix(data, cbTreeRfeN):
		tok := strings.TrimPrefix(data, cbTreeRfeN)
		oldPath := a.TreeState.GetPath(tok)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Kept original name",
			})
			return aErr
		})
		_ = a.telegramCall(ctx, chatID, func() error {
			name := filepath.Base(oldPath)
			if name == "." || name == "" {
				name = "file"
			}
			_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: msgID,
				RichMessage: &models.InputRichMessage{
					HTML: fmt.Sprintf("<p>Kept original name <code>%s</code>.</p>", html.EscapeString(name)),
				},
			})
			if editErr != nil && strings.Contains(editErr.Error(), "message can't be edited") {
				_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
					ChatID:    chatID,
					MessageID: msgID,
				})
				_, _ = b.SendRichMessage(ctx, &bot.SendRichMessageParams{
					ChatID: chatID,
					RichMessage: models.InputRichMessage{
						HTML: fmt.Sprintf("<p>Kept original name <code>%s</code>.</p>", html.EscapeString(name)),
					},
					ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
				})
				return nil
			}
			return editErr
		})
		_ = a.sendUpdatedTreeMessage(ctx, b, chatID, cq.From.ID)

	case strings.HasPrefix(data, cbTreeRfdY):
		parts := strings.Split(strings.TrimPrefix(data, cbTreeRfdY), ":")
		if len(parts) == 2 {
			withExt := a.TreeState.GetPath(parts[0])
			withoutExt := a.TreeState.GetPath(parts[1])
			if withExt != "" && withoutExt != "" {
				err := os.Rename(withExt, withoutExt)
				if err != nil {
					_ = a.telegramCall(ctx, chatID, func() error {
						_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
							CallbackQueryID: cq.ID,
							Text:            "Failed to delete format: " + err.Error(),
							ShowAlert:       true,
						})
						return aErr
					})
					return
				}
				a.TreeState.SetSelectedItem(cq.From.ID, withoutExt)
				_ = a.telegramCall(ctx, chatID, func() error {
					_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
						CallbackQueryID: cq.ID,
						Text:            "Format deleted. Renamed to " + filepath.Base(withoutExt),
					})
					return aErr
				})
				if treeMsgID := a.TreeState.GetUserMessageID(cq.From.ID); treeMsgID != 0 {
					_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, cq.From.ID)
				}
				_ = a.telegramCall(ctx, chatID, func() error {
					_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: msgID,
						RichMessage: &models.InputRichMessage{
							HTML: fmt.Sprintf("<p>Format deleted. File renamed to <code>%s</code> (without format).</p>", html.EscapeString(filepath.Base(withoutExt))),
						},
					})
					if editErr != nil && strings.Contains(editErr.Error(), "message can't be edited") {
						_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
							ChatID:    chatID,
							MessageID: msgID,
						})
						_, _ = b.SendRichMessage(ctx, &bot.SendRichMessageParams{
							ChatID: chatID,
							RichMessage: models.InputRichMessage{
								HTML: fmt.Sprintf("<p>Format deleted. File renamed to <code>%s</code> (without format).</p>", html.EscapeString(filepath.Base(withoutExt))),
							},
							ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
						})
						return nil
					}
					return editErr
				})
				_ = a.sendUpdatedTreeMessage(ctx, b, chatID, cq.From.ID)
			}
		}

	case strings.HasPrefix(data, cbTreeRfdN):
		tok := strings.TrimPrefix(data, cbTreeRfdN)
		withExt := a.TreeState.GetPath(tok)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, aErr := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Kept format",
			})
			return aErr
		})
		_ = a.telegramCall(ctx, chatID, func() error {
			name := filepath.Base(withExt)
			if name == "." || name == "" {
				name = "file"
			}
			_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: msgID,
				RichMessage: &models.InputRichMessage{
					HTML: fmt.Sprintf("<p>Kept format for <code>%s</code>.</p>", html.EscapeString(name)),
				},
			})
			if editErr != nil && strings.Contains(editErr.Error(), "message can't be edited") {
				_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
					ChatID:    chatID,
					MessageID: msgID,
				})
				_, _ = b.SendRichMessage(ctx, &bot.SendRichMessageParams{
					ChatID: chatID,
					RichMessage: models.InputRichMessage{
						HTML: fmt.Sprintf("<p>Kept format for <code>%s</code>.</p>", html.EscapeString(name)),
					},
					ReplyMarkup: a.currentReplyMarkup(cq.From.ID),
				})
				return nil
			}
			return editErr
		})
		_ = a.sendUpdatedTreeMessage(ctx, b, chatID, cq.From.ID)
	}
}

// editTreeMessage updates the tree rich message in-place using rich_message.
func (a *App) editTreeMessage(ctx context.Context, b *bot.Bot, chatID int64, messageID int, userID int64) error {
	curPath := a.TreeState.GetUserPath(userID)
	richHTML, err := a.buildTreeRichMessageHTML(curPath, userID)
	if err != nil {
		a.Log.Error("failed to build tree html", "err", err, "path", curPath)
		return err
	}
	var editErr error
	callErr := a.telegramCall(ctx, chatID, func() error {
		_, editErr = b.EditMessageText(ctx, &bot.EditMessageTextParams{
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
	if callErr == nil {
		return nil
	}

	a.Log.Warn("editTreeMessage EditMessageText failed", "err", callErr, "chatID", chatID, "messageID", messageID)

	// Fallback: If Telegram refuses to edit the message (e.g. message can't be edited because it had reply_markup,
	// or message was deleted / not found), delete the stale message if possible and send a fresh editable tree message.
	if strings.Contains(callErr.Error(), "message can't be edited") ||
		strings.Contains(callErr.Error(), "message to edit not found") {
		_ = a.telegramCall(ctx, chatID, func() error {
			_, dErr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    chatID,
				MessageID: messageID,
			})
			return dErr
		})
		return a.sendUpdatedTreeMessage(ctx, b, chatID, userID)
	}

	return callErr
}

// sendUpdatedTreeMessage sends a fresh tree message reflecting current explorer state.
func (a *App) sendUpdatedTreeMessage(ctx context.Context, b *bot.Bot, chatID, userID int64) error {
	curPath := a.TreeState.GetUserPath(userID)
	if curPath == "" {
		curPath = a.TreeState.Home()
	}
	richHTML, err := a.buildTreeRichMessageHTML(curPath, userID)
	if err != nil {
		return err
	}
	return a.telegramCall(ctx, chatID, func() error {
		msg, sendErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
			ChatID: chatID,
			RichMessage: models.InputRichMessage{
				HTML: richHTML,
			},
		})
		if sendErr == nil && msg != nil {
			a.TreeState.SetUserMessage(userID, msg.ID)
		}
		return sendErr
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
			if err != nil && strings.Contains(err.Error(), "message can't be edited") {
				_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
					ChatID:    chatID,
					MessageID: treeMsgID,
				})
				_, _ = b.SendRichMessage(ctx, &bot.SendRichMessageParams{
					ChatID: chatID,
					RichMessage: models.InputRichMessage{
						HTML: fmt.Sprintf("<p>🌳 <b>Folder Explorer</b> (closed)<br/><i>Last path:</i> <code>%s</code></p>", html.EscapeString(cur)),
					},
					ReplyMarkup: a.currentReplyMarkup(userID),
				})
				return nil
			}
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
	sb.WriteString("<p>🌳 <b>Folder Explorer</b></p>\n")

	sb.WriteString("<p>\n")
	lineCount := 0
	a.renderDirTree(&sb, cleanPath, "", userID, &lineCount)
	sb.WriteString("</p>\n")

	// If an item (folder or file) has been clicked, add action buttons in a row preceded by a divider line
	if sel := a.TreeState.GetSelectedItem(userID); sel != "" {
		if fi, err := os.Stat(sel); err == nil {
			tok := a.TreeState.GetToken(sel)
			sb.WriteString("<br/>\n<hr/>\n")
			home := ""
			if a.TreeState != nil {
				home = a.TreeState.Home()
			}
			tildePath := formatTildePath(sel, home, cleanPath)
			sb.WriteString("<p><code>" + html.EscapeString(tildePath) + "</code></p>\n")
			sb.WriteString("<tg-button-row align=\"left\">\n")
			sb.WriteString(fmt.Sprintf("  <tg-button type=\"copy_text\" style=\"success\" text=\"%s\">%s</tg-button>\n",
				html.EscapeString(sel), copyButtonLabel(sel)))
			if fi.IsDir() {
				sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:zip:%s\">download folder</tg-button>\n", tok))
				sb.WriteString("</tg-button-row>")
			} else {
				sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:send:%s\">download file</tg-button>\n", tok))
				sb.WriteString("</tg-button-row>\n")
				sb.WriteString("<hr/>\n")
				sb.WriteString("<p>file operations:</p>\n")
				sb.WriteString("<tg-button-row align=\"left\">\n")
				sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:ren:%s\">rename</tg-button>\n", tok))
				sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:edit:%s\">edit</tg-button>\n", tok))
				sb.WriteString("</tg-button-row>")
			}
		}
	}

	return sb.String(), nil
}

// renderDirTree recursively renders directory contents, expanding only folders toggled by user.
func (a *App) renderDirTree(sb *strings.Builder, dirPath string, prefix string, userID int64, lineCount *int) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		sb.WriteString("\u200b" + prefix + "<i>(cannot read directory)</i><br/>\n")
		return
	}

	selectedItem := a.TreeState.GetSelectedItem(userID)

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
		sb.WriteString("\u200b" + prefix + "<i>(empty)</i><br/>\n")
		return
	}

	for i, d := range dirs {
		if *lineCount >= treeMaxLines {
			sb.WriteString("\u200b" + prefix + "<i>... (more items truncated)</i><br/>\n")
			return
		}

		isLast := (i == len(dirs)-1 && len(files) == 0)
		branch := "├"
		nextPrefix := prefix + "│"
		if isLast {
			branch = "└"
			nextPrefix = prefix + "\u00a0\u00a0"
		}

		subPath := filepath.Join(dirPath, d.Name())
		tok := a.TreeState.GetToken(subPath)
		isOpen := a.TreeState.IsExpanded(userID, subPath)

		folderName := d.Name()
		if isOpen {
			folderName += "/"
		}

		cursor := ""
		if subPath == selectedItem {
			cursor = " 👈"
		}

		sb.WriteString(fmt.Sprintf("\u200b%s%s<tg-button type=\"callback_data\" style=\"primary\" data=\"t:tog:%s\">%s</tg-button>%s<br/>\n",
			prefix, branch, tok, html.EscapeString(folderName), cursor))
		(*lineCount)++

		if isOpen {
			a.renderDirTree(sb, subPath, nextPrefix, userID, lineCount)
		}
	}

	for i, f := range files {
		if *lineCount >= treeMaxLines {
			sb.WriteString("\u200b" + prefix + "<i>... (more files truncated)</i><br/>\n")
			return
		}

		branch := "├"
		if i == len(files)-1 {
			branch = "└"
		}

		filePath := filepath.Join(dirPath, f.Name())
		tok := a.TreeState.GetToken(filePath)

		cursor := ""
		if filePath == selectedItem {
			cursor = " 👈"
		}

		sb.WriteString(fmt.Sprintf("\u200b%s%s<tg-button type=\"callback_data\" data=\"t:file:%s\">%s</tg-button>%s<br/>\n",
			prefix, branch, tok, html.EscapeString(f.Name()), cursor))
		(*lineCount)++
	}
}

func sortEntries(entries []os.DirEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
}

func formatFileSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatTildePath formats a path starting with ~/ if it resides in home or root.
func formatTildePath(path, home, root string) string {
	cleanPath := filepath.Clean(path)
	for _, base := range []string{root, home} {
		if base == "" {
			continue
		}
		cleanBase := filepath.Clean(base)
		if cleanPath == cleanBase {
			return "~/"
		}
		if strings.HasPrefix(cleanPath, cleanBase+string(filepath.Separator)) {
			rel := strings.TrimPrefix(cleanPath, cleanBase+string(filepath.Separator))
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(cleanPath)
}

func copyButtonLabel(path string) string {
	return "copy full path"
}

// buildFileRichMessageHTML formats the message sent when a file is selected.
func buildFileRichMessageHTML(filePath, tok string, size int64) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<p>📄 <b>%s</b> (%s)<br/>\n", html.EscapeString(filepath.Base(filePath)), formatFileSize(size)))
	sb.WriteString("<code>" + html.EscapeString(filePath) + "</code></p>\n")
	sb.WriteString("<br/>\n<hr/>\n")
	sb.WriteString("<tg-button-row align=\"left\">\n")
	sb.WriteString(fmt.Sprintf("  <tg-button type=\"copy_text\" style=\"success\" text=\"%s\">%s</tg-button>\n",
		html.EscapeString(filePath), copyButtonLabel(filePath)))
	sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:send:%s\">download file</tg-button>\n", tok))
	sb.WriteString("</tg-button-row>\n")
	sb.WriteString("<hr/>\n")
	sb.WriteString("<p>file operations:</p>\n")
	sb.WriteString("<tg-button-row align=\"left\">\n")
	sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:ren:%s\">rename</tg-button>\n", tok))
	sb.WriteString(fmt.Sprintf("  <tg-button type=\"callback_data\" style=\"success\" data=\"t:edit:%s\">edit</tg-button>\n", tok))
	sb.WriteString("</tg-button-row>")
	return sb.String()
}

// createZipArchive creates a zip archive in memory of targetPath up to maxBytes.
func createZipArchive(targetPath string, maxBytes int64) ([]byte, error) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	baseDir := filepath.Base(targetPath)

	err := filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden directories and hidden files (except targetPath itself)
		if path != targetPath && strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(targetPath, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		if int64(buf.Len()) > maxBytes {
			return fmt.Errorf("archive exceeds maximum size (%d MB)", maxBytes/(1024*1024))
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(filepath.Join(baseDir, relPath))
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			lr := io.LimitReader(file, maxBytes+1)
			written, copyErr := io.Copy(writer, lr)
			_ = file.Close()
			if copyErr != nil {
				return copyErr
			}
			if written > maxBytes || int64(buf.Len()) > maxBytes {
				return fmt.Errorf("archive exceeds maximum size (%d MB)", maxBytes/(1024*1024))
			}
		}

		return nil
	})

	if err != nil {
		_ = zw.Close()
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	if int64(buf.Len()) > maxBytes {
		return nil, fmt.Errorf("archive exceeds maximum size (%d MB)", maxBytes/(1024*1024))
	}

	return buf.Bytes(), nil
}

// handleTreeMessage checks if user is awaiting tree input (edit/rename) and processes it.
// Returns true if the message was handled.
func (a *App) handleTreeMessage(ctx context.Context, b *bot.Bot, msg *models.Message) bool {
	if msg == nil || msg.From == nil {
		return false
	}
	userID := msg.From.ID
	chatID := msg.Chat.ID
	aw, ok := a.TreeState.GetAwaiting(userID)
	if !ok || aw.Action == TreeActionNone {
		return false
	}

	text := strings.TrimSpace(msg.Text)
	if text == "/cancel" {
		a.TreeState.ClearAwaiting(userID)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      chatID,
				Text:        "Action cancelled.",
				ReplyMarkup: a.currentReplyMarkup(userID),
			})
			return err
		})
		return true
	}
	if text == btnHome || text == btnBack || text == btnCancel {
		a.TreeState.ClearAwaiting(userID)
		return false
	}

	switch aw.Action {
	case TreeActionEdit:
		a.handleTreeEditInput(ctx, b, chatID, userID, aw, msg)
		return true

	case TreeActionRename:
		a.handleTreeRenameInput(ctx, b, chatID, userID, aw, msg)
		return true
	}

	return false
}

func (a *App) handleTreeEditInput(ctx context.Context, b *bot.Bot, chatID, userID int64, aw TreeAwaiting, msg *models.Message) {
	if msg.Document != nil {
		uploadedName := msg.Document.FileName
		if uploadedName == "" {
			uploadedName = filepath.Base(aw.TargetPath)
		}
		origExt := filepath.Ext(aw.TargetPath)
		uploadedExt := filepath.Ext(uploadedName)
		if !strings.EqualFold(origExt, uploadedExt) {
			a.TreeState.ClearAwaiting(userID)
			_ = a.telegramCall(ctx, chatID, func() error {
				_, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   fmt.Sprintf("Format mismatch: expected %q, got %q. Content not modified.", origExt, uploadedExt),
				})
				return err
			})
			return
		}

		f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: msg.Document.FileID})
		if err != nil {
			a.Log.Error("failed to getFile", "err", err)
			a.showStatus(ctx, b, chatID, userID, "Failed to download file from Telegram: "+err.Error())
			return
		}
		downloadURL := b.FileDownloadLink(f)
		client := a.httpClient
		if client == nil {
			client = http.DefaultClient
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to build download request: "+err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to download file: "+err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			a.showStatus(ctx, b, chatID, userID, fmt.Sprintf("Download failed: HTTP %d", resp.StatusCode))
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSendSize+1))
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to read file data: "+err.Error())
			return
		}
		if int64(len(data)) > maxFileSendSize {
			a.showStatus(ctx, b, chatID, userID, "File is too large (> 20 MB).")
			return
		}

		err = os.WriteFile(aw.TargetPath, data, 0o644)
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to write file: "+err.Error())
			return
		}
		a.TreeState.ClearAwaiting(userID)
		if treeMsgID := a.TreeState.GetUserMessageID(userID); treeMsgID != 0 {
			_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, userID)
		}

		if uploadedName == filepath.Base(aw.TargetPath) {
			_ = a.telegramCall(ctx, chatID, func() error {
				_, sErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
					ChatID: chatID,
					RichMessage: models.InputRichMessage{
						HTML: fmt.Sprintf("<p>File <code>%s</code> content updated successfully.</p>", html.EscapeString(uploadedName)),
					},
					ReplyMarkup: a.currentReplyMarkup(userID),
				})
				return sErr
			})
			_ = a.sendUpdatedTreeMessage(ctx, b, chatID, userID)
			return
		}

		// "If the user response was a file, ask user if they wish to rename the file aswell."
		targetDir := filepath.Dir(aw.TargetPath)
		newTargetPath := filepath.Join(targetDir, uploadedName)
		tokOld := a.TreeState.GetToken(aw.TargetPath)
		tokNew := a.TreeState.GetToken(newTargetPath)
		oldName := filepath.Base(aw.TargetPath)
		richHTML := fmt.Sprintf(
			"<p>✅ File content replaced.</p>\n<p>Rename the file too?</p>\n<p><code>Old: </code><code>%s</code> 👇<br/><code>New: </code><code>%s</code></p>\n<tg-button-row align=\"left\">\n  <tg-button type=\"callback_data\" style=\"success\" data=\"%s%s:%s\">✅ Yes</tg-button>\n  <tg-button type=\"callback_data\" style=\"danger\" data=\"%s%s\">❌ No</tg-button>\n</tg-button-row>",
			html.EscapeString(oldName),
			html.EscapeString(uploadedName),
			cbTreeRfeY, tokOld, tokNew,
			cbTreeRfeN, tokOld,
		)
		_ = a.telegramCall(ctx, chatID, func() error {
			_, sErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
				ChatID: chatID,
				RichMessage: models.InputRichMessage{
					HTML: richHTML,
				},
			})
			return sErr
		})
		return
	}

	if msg.Text != "" {
		err := os.WriteFile(aw.TargetPath, []byte(msg.Text), 0o644)
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to write file: "+err.Error())
			return
		}
		a.TreeState.ClearAwaiting(userID)
		if treeMsgID := a.TreeState.GetUserMessageID(userID); treeMsgID != 0 {
			_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, userID)
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, sErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
				ChatID: chatID,
				RichMessage: models.InputRichMessage{
					HTML: fmt.Sprintf("<p>File <code>%s</code> content updated successfully.</p>", html.EscapeString(filepath.Base(aw.TargetPath))),
				},
				ReplyMarkup: a.currentReplyMarkup(userID),
			})
			return sErr
		})
		_ = a.sendUpdatedTreeMessage(ctx, b, chatID, userID)
		return
	}

	a.showStatus(ctx, b, chatID, userID, "Please send only a text message or a file with the same format.")
}

func (a *App) handleTreeRenameInput(ctx context.Context, b *bot.Bot, chatID, userID int64, aw TreeAwaiting, msg *models.Message) {
	rawName := strings.TrimSpace(msg.Text)
	if rawName == "" {
		a.showStatus(ctx, b, chatID, userID, "Please send a text message with the new name.")
		return
	}

	if rawName == "." || rawName == ".." || strings.Contains(rawName, "/") || strings.Contains(rawName, "\\") {
		a.showStatus(ctx, b, chatID, userID, "Invalid file name.")
		return
	}

	origExt := filepath.Ext(aw.TargetPath)
	newExt := filepath.Ext(rawName)
	targetDir := filepath.Dir(aw.TargetPath)

	if newExt != "" || origExt == "" {
		finalPath := filepath.Join(targetDir, rawName)
		err := os.Rename(aw.TargetPath, finalPath)
		if err != nil {
			a.showStatus(ctx, b, chatID, userID, "Failed to rename: "+err.Error())
			return
		}
		a.TreeState.SetSelectedItem(userID, finalPath)
		a.TreeState.ClearAwaiting(userID)
		if treeMsgID := a.TreeState.GetUserMessageID(userID); treeMsgID != 0 {
			_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, userID)
		}
		_ = a.telegramCall(ctx, chatID, func() error {
			_, sErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
				ChatID: chatID,
				RichMessage: models.InputRichMessage{
					HTML: fmt.Sprintf("<p>File renamed to <code>%s</code>.</p>", html.EscapeString(rawName)),
				},
				ReplyMarkup: a.currentReplyMarkup(userID),
			})
			return sErr
		})
		_ = a.sendUpdatedTreeMessage(ctx, b, chatID, userID)
		return
	}

	// newExt == "" && origExt != ""
	// "If the response doesn't have format, ask them that the file got renamed with the same format and if they wish to delete the format as well (to rename it again without format)"
	nameWithExt := rawName + origExt
	pathWithExt := filepath.Join(targetDir, nameWithExt)
	pathWithoutExt := filepath.Join(targetDir, rawName)

	err := os.Rename(aw.TargetPath, pathWithExt)
	if err != nil {
		a.showStatus(ctx, b, chatID, userID, "Failed to rename: "+err.Error())
		return
	}

	a.TreeState.SetSelectedItem(userID, pathWithExt)
	a.TreeState.ClearAwaiting(userID)
	if treeMsgID := a.TreeState.GetUserMessageID(userID); treeMsgID != 0 {
		_ = a.editTreeMessage(ctx, b, chatID, treeMsgID, userID)
	}

	tokWithExt := a.TreeState.GetToken(pathWithExt)
	tokWithoutExt := a.TreeState.GetToken(pathWithoutExt)
	richHTML := fmt.Sprintf(
		"<p>File got renamed to <code>%s</code> with the same format (<code>%s</code>).</p>\n<p>Do you wish to delete the format as well (rename to <code>%s</code> without format)?</p>\n<tg-button-row align=\"left\">\n  <tg-button type=\"callback_data\" style=\"danger\" data=\"%s%s:%s\">🗑 Delete format</tg-button>\n  <tg-button type=\"callback_data\" style=\"success\" data=\"%s%s\">Keep format</tg-button>\n</tg-button-row>",
		html.EscapeString(nameWithExt),
		html.EscapeString(origExt),
		html.EscapeString(rawName),
		cbTreeRfdY, tokWithExt, tokWithoutExt,
		cbTreeRfdN, tokWithExt,
	)
	_ = a.telegramCall(ctx, chatID, func() error {
		_, sErr := b.SendRichMessage(ctx, &bot.SendRichMessageParams{
			ChatID: chatID,
			RichMessage: models.InputRichMessage{
				HTML: richHTML,
			},
		})
		return sErr
	})
}
