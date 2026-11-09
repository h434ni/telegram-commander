package bot_test

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/require"

	"github.com/azolfagharj/telegram-commander/internal/bot"
	"github.com/azolfagharj/telegram-commander/internal/config"
	"github.com/azolfagharj/telegram-commander/internal/executor"
	"github.com/azolfagharj/telegram-commander/internal/function"
)

type apiServer struct {
	mu              sync.Mutex
	messages        []map[string]any
	documents       []map[string]any
	callbackAnswers []map[string]any
	deletes         int
	deletedIDs      []string

	// Optional failure injection for delivery-retry tests.
	// failSendN: first N sendMessage/sendDocument calls return an error.
	// failSendCode: Telegram error_code (default 500). Use 403/400 for permanent.
	// failRetryAfter: parameters.retry_after when failSendCode is 429.
	failSendN      int
	failSendCode   int
	failRetryAfter int
	sendAttempts   int

	failEditMessageWithUneditable bool
}

func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = r.ParseMultipartForm(32 << 20)

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/getMe"):
		writeOK(w, map[string]any{
			"id": 1, "is_bot": true, "first_name": "Test", "username": "testbot",
		})
	case strings.HasSuffix(path, "/sendMessage"):
		if s.injectSendFailure(w) {
			return
		}
		id := len(s.messages) + 1
		m := map[string]any{
			"chat_id":          r.FormValue("chat_id"),
			"text":             r.FormValue("text"),
			"reply_markup":     r.FormValue("reply_markup"),
			"reply_parameters": r.FormValue("reply_parameters"),
			"parse_mode":       r.FormValue("parse_mode"),
			"message_id":       id,
			"kind":             "send",
		}
		s.messages = append(s.messages, m)
		writeOK(w, map[string]any{
			"message_id": id,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 10, "type": "private"},
			"text":       m["text"],
		})
	case strings.HasSuffix(path, "/sendRichMessage"):
		if s.injectSendFailure(w) {
			return
		}
		id := len(s.messages) + 1
		text := r.FormValue("text")
		if text == "" && r.FormValue("rich_message") != "" {
			var rm struct {
				HTML string `json:"html"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("rich_message")), &rm)
			text = rm.HTML
		}
		m := map[string]any{
			"chat_id":      r.FormValue("chat_id"),
			"text":         text,
			"rich_message": r.FormValue("rich_message"),
			"reply_markup": r.FormValue("reply_markup"),
			"message_id":   id,
			"kind":         "send_rich",
		}
		s.messages = append(s.messages, m)
		writeOK(w, map[string]any{
			"message_id": id,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 10, "type": "private"},
			"text":       m["text"],
		})
	case strings.HasSuffix(path, "/answerCallbackQuery"):
		s.callbackAnswers = append(s.callbackAnswers, map[string]any{
			"id":         r.FormValue("callback_query_id"),
			"text":       r.FormValue("text"),
			"show_alert": r.FormValue("show_alert"),
		})
		writeOK(w, true)
	case strings.HasSuffix(path, "/editMessageText"):
		if s.failEditMessageWithUneditable {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`))
			return
		}
		text := r.FormValue("text")
		if text == "" && r.FormValue("rich_message") != "" {
			var rm struct {
				HTML string `json:"html"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("rich_message")), &rm)
			text = rm.HTML
		}
		m := map[string]any{
			"chat_id":      r.FormValue("chat_id"),
			"message_id":   r.FormValue("message_id"),
			"text":         text,
			"rich_message": r.FormValue("rich_message"),
			"kind":         "edit",
		}
		s.messages = append(s.messages, m)
		writeOK(w, map[string]any{
			"message_id": 1,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 10, "type": "private"},
			"text":       m["text"],
		})
	case strings.HasSuffix(path, "/deleteMessage"):
		s.deletes++
		s.deletedIDs = append(s.deletedIDs, r.FormValue("message_id"))
		writeOK(w, true)
	case strings.HasSuffix(path, "/sendDocument"):
		if s.injectSendFailure(w) {
			return
		}
		id := len(s.messages) + len(s.documents) + 1
		var filename string
		var size int
		if r.MultipartForm != nil {
			if files := r.MultipartForm.File["document"]; len(files) > 0 {
				filename = files[0].Filename
				size = int(files[0].Size)
				f, err := files[0].Open()
				if err == nil {
					defer f.Close()
					buf := make([]byte, size)
					n, _ := f.Read(buf)
					size = n
					_ = buf
				}
			}
		}
		doc := map[string]any{
			"chat_id":      r.FormValue("chat_id"),
			"caption":      r.FormValue("caption"),
			"reply_markup": r.FormValue("reply_markup"),
			"filename":     filename,
			"size":         size,
			"message_id":   id,
			"kind":         "document",
		}
		s.documents = append(s.documents, doc)
		writeOK(w, map[string]any{
			"message_id": id,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": 10, "type": "private"},
			"document":   map[string]any{"file_name": filename, "file_size": size},
			"caption":    doc["caption"],
		})
	case strings.HasSuffix(path, "/getFile"):
		fileID := r.FormValue("file_id")
		writeOK(w, map[string]any{
			"file_id":   fileID,
			"file_size": 17,
			"file_path": "mock/" + fileID,
		})
	case strings.Contains(path, "/file/bot"):
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock-file-content"))
	default:
		writeOK(w, true)
	}
}

// injectSendFailure returns true when the request was answered with an error.
// Caller must hold s.mu.
func (s *apiServer) injectSendFailure(w http.ResponseWriter) bool {
	s.sendAttempts++
	if s.failSendN <= 0 {
		return false
	}
	s.failSendN--
	code := s.failSendCode
	if code == 0 {
		code = 500
	}
	writeAPIError(w, code, s.failRetryAfter)
	return true
}

func writeOK(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// writeAPIError returns a Telegram-shaped error body. The go-telegram/bot
// client keys off error_code in JSON, not the HTTP status.
func writeAPIError(w http.ResponseWriter, code, retryAfter int) {
	body := map[string]any{
		"ok":          false,
		"error_code":  code,
		"description": "injected test failure",
	}
	if code == http.StatusTooManyRequests {
		body["parameters"] = map[string]any{"retry_after": retryAfter}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// tinyDeliveryRetry sets very short retry waits so integration tests stay fast.
func tinyDeliveryRetry(cfg *config.Config) {
	cfg.DeliveryRetryBackoff = config.Duration{Duration: time.Millisecond}
	cfg.DeliveryRetryBackoffMax = config.Duration{Duration: 2 * time.Millisecond}
	cfg.DeliveryRetryTTL = config.Duration{Duration: 500 * time.Millisecond}
}

func startCommandUpdate(userID int64, username, text string) *models.Update {
	return &models.Update{
		ID: 1,
		Message: &models.Message{
			ID:   1,
			Date: int(time.Now().Unix()),
			Chat: models.Chat{ID: 10, Type: models.ChatTypePrivate},
			From: &models.User{ID: userID, FirstName: "U", Username: username},
			Text: text,
			Entities: []models.MessageEntity{
				{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: len(strings.SplitN(text, " ", 2)[0])},
			},
		},
	}
}

func textUpdate(userID int64, username, text string) *models.Update {
	return &models.Update{
		ID: 2,
		Message: &models.Message{
			ID:   2,
			Date: int(time.Now().Unix()),
			Chat: models.Chat{ID: 10, Type: models.ChatTypePrivate},
			From: &models.User{ID: userID, FirstName: "U", Username: username},
			Text: text,
		},
	}
}

func replyTextUpdate(userID int64, username, text string, replyToID int) *models.Update {
	up := textUpdate(userID, username, text)
	up.Message.ReplyToMessage = &models.Message{ID: replyToID}
	return up
}

func replyDocumentUpdate(userID int64, username, filename, fileID string, replyToID int) *models.Update {
	up := textUpdate(userID, username, "")
	up.Message.ReplyToMessage = &models.Message{ID: replyToID}
	up.Message.Document = &models.Document{
		FileID:   fileID,
		FileName: filename,
		FileSize: 100,
	}
	return up
}

func callbackUpdate(userID int64, data string) *models.Update {
	return &models.Update{
		ID: 3,
		CallbackQuery: &models.CallbackQuery{
			ID:   "cb1",
			From: models.User{ID: userID, FirstName: "U", Username: "admin"},
			Data: data,
			Message: models.MaybeInaccessibleMessage{
				Type: models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{
					ID:   1,
					Date: int(time.Now().Unix()),
					Chat: models.Chat{ID: 10, Type: models.ChatTypePrivate},
				},
			},
		},
	}
}

func visibleTexts(s *apiServer) []string {
	var texts []string
	for _, m := range s.messages {
		t, ok := m["text"].(string)
		if !ok || t == "" || t == "\u2060" || t == "\u2800" {
			continue
		}
		texts = append(texts, t)
	}
	return texts
}

func TestUnauthorizedUserGetsDenyMessage(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"999"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(1, "intruder", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.NotEmpty(t, srv.messages)
	text, _ := srv.messages[0]["text"].(string)
	require.Contains(t, text, "not allowed")
	require.Contains(t, text, "1")
	require.Contains(t, text, "intruder")
}

func TestAllowedStartSendsMenu(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42", "admin"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, visibleTexts(srv), "Menu")
}

func TestInlineMenuNavigation(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name: "Cat",
				Type: "category",
				Icon: "📁",
				Items: []config.ButtonNode{
					{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0]))
	b.ProcessUpdate(ctx, callbackUpdate(42, "h"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.NotEmpty(t, srv.messages)
	texts := visibleTexts(srv)
	require.Contains(t, texts, "Menu")
	foundCat := false
	for _, line := range texts {
		if strings.Contains(line, "Cat") {
			foundCat = true
			break
		}
	}
	require.True(t, foundCat)
	require.Equal(t, "Menu", texts[len(texts)-1])
}

func TestMenuAlwaysSendsFreshMessageAndCleansUpOld(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name: "Cat",
				Type: "category",
				Items: []config.ButtonNode{
					{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0]))
	b.ProcessUpdate(ctx, callbackUpdate(42, "h"))

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Every screen (Menu, Cat, Menu) must be a brand new sendMessage call,
	// never an editMessageText, so Telegram never plays its keyboard
	// "menu transition" resize animation that can make button text look
	// cut off or overlapping on Android.
	require.Equal(t, 3, len(visibleTexts(srv)), "each screen should be its own new message")
	// Each screen after the first deletes the previous menu message, so the
	// chat never fills up with old screens.
	require.Equal(t, 2, srv.deletes, "old menu messages should be cleaned up")
}

func TestCommandRunKeepsRunningAndResultAfterHome(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{ExitCode: 0, Stdout: "hello-out"}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Echo"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🏠 Home"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	texts := visibleTexts(srv)
	require.Contains(t, strings.Join(texts, "\n"), "Running: Echo")
	require.Contains(t, strings.Join(texts, "\n"), "Button: Echo")
	require.Contains(t, strings.Join(texts, "\n"), "hello-out")
	require.Equal(t, "Menu", texts[len(texts)-1])
	// Start menu is removed when the command starts; Home does not remove Running or the result.
	require.Equal(t, 1, srv.deletes)
}

func TestResultKeepsCategoryMenuKeyboard(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{ExitCode: 0, Stdout: "ok"}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name: "Cat",
				Type: "category",
				Items: []config.ButtonNode{
					{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0]))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Echo"))

	srv.mu.Lock()
	var lastResult map[string]any
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "<pre>") {
			lastResult = m
		}
	}
	require.NotNil(t, lastResult)
	rm := fmt.Sprintf("%v", lastResult["reply_markup"])
	require.Contains(t, rm, "Echo")
	require.Contains(t, rm, "Back")
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🔙 Back"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	texts := visibleTexts(srv)
	require.Equal(t, "Menu", texts[len(texts)-1], "Back from a root category should open Home")
}

func TestMenuMessageCarriesHomeReplyKeyboard(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// No separate near-blank message should be sent anymore; the Home button
	// now lives in the reply keyboard attached to the menu message itself.
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		require.NotEqual(t, "\u2800", text, "no blank keyboard-priming message should be sent")
	}

	// The menu message must carry a reply keyboard (shown under the message
	// box) that includes the Home button.
	require.NotEmpty(t, srv.messages)
	rm := fmt.Sprintf("%v", srv.messages[0]["reply_markup"])
	require.Contains(t, rm, "keyboard")
	require.Contains(t, rm, "Home")
}

func TestTypedStartAfterTapSendsNewMessage(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name: "Cat",
				Type: "category",
				Items: []config.ButtonNode{
					{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0])) // a button tap: edits in place
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))   // typed again: must be a new message

	srv.mu.Lock()
	defer srv.mu.Unlock()

	last := srv.messages[len(srv.messages)-1]
	require.Equal(t, "send", last["kind"], "the menu after a typed command must be a new message, not an edit of an older one")
	require.Equal(t, "Menu", last["text"])
}

func TestRunCommandPromptAndExecutesNextText(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var gotCommand string
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, spec executor.Spec) (executor.Result, error) {
			gotCommand = spec.Command
			return executor.Result{ExitCode: 0, Stdout: "ok"}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		EnableRunCommand: true,
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "$ >_ Run Command"))

	srv.mu.Lock()
	require.Contains(t, visibleTexts(srv), "Type your command and send it.")
	rm := fmt.Sprintf("%v", srv.messages[len(srv.messages)-1]["reply_markup"])
	require.Contains(t, rm, "Run Command")
	require.NotContains(t, rm, "Echo")
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "echo hello-raw"))

	require.Equal(t, "echo hello-raw", gotCommand)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	joined := strings.Join(visibleTexts(srv), "\n")
	require.Contains(t, joined, "Button: Run Command")
}

func TestButtonWithPromptTakesInput(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var gotCommand string
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, spec executor.Spec) (executor.Result, error) {
			gotCommand = spec.Command
			return executor.Result{ExitCode: 0, Stdout: "ping ok"}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name:     "Ping Host",
				Type:     "button",
				Function: "command",
				Prompt:   "Enter hostname or IP to ping:",
				Command:  "ping -c 4 {{.input}}",
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Ping Host"))

	srv.mu.Lock()
	require.Contains(t, visibleTexts(srv), "Enter hostname or IP to ping:")
	lastMsg := srv.messages[len(srv.messages)-1]
	rm := fmt.Sprintf("%v", lastMsg["reply_markup"])
	require.Contains(t, rm, "force_reply")
	require.Contains(t, rm, "Enter hostname or IP to ping:")
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "1.1.1.1"))

	require.Equal(t, "ping -c 4 1.1.1.1", gotCommand)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	joined := strings.Join(visibleTexts(srv), "\n")
	require.Contains(t, joined, "Button: Ping Host")
	require.Contains(t, joined, "ping ok")
}

func TestButtonWithPromptAndConfirm(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var gotCommand string
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, spec executor.Spec) (executor.Result, error) {
			gotCommand = spec.Command
			return executor.Result{ExitCode: 0, Stdout: "reboot ok"}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name:     "Reboot Server",
				Type:     "button",
				Function: "command",
				Prompt:   "Reason for reboot:",
				Confirm:  true,
				Command:  "echo rebooting: {{.input}}",
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Reboot Server"))

	srv.mu.Lock()
	require.Contains(t, visibleTexts(srv), "Reason for reboot:")
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "kernel update"))

	srv.mu.Lock()
	require.Contains(t, visibleTexts(srv), "Confirm: Reboot Server (kernel update) ?")
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "✅ Yes"))

	require.Equal(t, "echo rebooting: kernel update", gotCommand)
}

func TestRunCommandHomeCancelsAwaiting(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ran := false
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			ran = true
			return executor.Result{ExitCode: 0}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		EnableRunCommand: true,
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "$ >_ Run Command"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🏠 Home"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "echo hello-raw"))

	require.False(t, ran)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, visibleTexts(srv)[len(visibleTexts(srv))-1], "Unknown option")
}

func TestRunCommandBackCancelsAwaiting(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ran := false
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			ran = true
			return executor.Result{ExitCode: 0}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		EnableRunCommand: true,
		Menu: []config.ButtonNode{
			{
				Name: "Cat",
				Type: "category",
				Items: []config.ButtonNode{
					{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0]))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "$ >_ Run Command"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🔙 Back"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "echo hello-raw"))

	require.False(t, ran)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, visibleTexts(srv)[len(visibleTexts(srv))-1], "Unknown option")
}

func TestRunCommandButtonAbsentWhenDisabled(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	rm := fmt.Sprintf("%v", srv.messages[0]["reply_markup"])
	require.NotContains(t, rm, "Run Command")
}

func TestLongCommandResultIsSplitAndReplied(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var stdout strings.Builder
	line := `udp UNCONN 0 0 0.0.0.0:80 0.0.0.0:* users:(("python3",pid=1,fd=2))`
	for i := 0; i < 80; i++ {
		stdout.WriteString(line)
		stdout.WriteByte('\n')
	}

	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{ExitCode: 0, Stdout: stdout.String()}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi", Output: "text"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Echo"))

	srv.mu.Lock()
	var result []map[string]any
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "<pre>") {
			result = append(result, m)
		}
	}
	require.Greater(t, len(result), 1, "long output should be more than one message")
	for i, m := range result {
		text, _ := m["text"].(string)
		require.LessOrEqual(t, len(text), 4096, "chunk %d", i)
		require.Contains(t, text, "</pre>")
		require.Equal(t, "HTML", m["parse_mode"])
	}
	last := result[len(result)-1]
	rm := fmt.Sprintf("%v", last["reply_markup"])
	require.Contains(t, rm, "Home")
	require.Contains(t, rm, "Echo")
	firstRM := fmt.Sprintf("%v", result[0]["reply_markup"])
	require.NotContains(t, firstRM, "Home")
	reply := fmt.Sprintf("%v", result[1]["reply_parameters"])
	require.Contains(t, reply, "message_id")
	firstChunk, _ := result[0]["text"].(string)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🏠 Home"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	var still []string
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "<pre>") {
			still = append(still, text)
		}
	}
	require.Greater(t, len(still), 1)
	require.Equal(t, firstChunk, still[0], "first output chunk should still be in the chat after Home")
}

func newParamIntegrationBot(t *testing.T, cfg *config.Config) (*bot.App, *tgbot.Bot, *string) {
	t.Helper()
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	cfg.Telegram.API = ts.URL
	cfg.Telegram.BotToken = "123:ABC"
	cfg.Telegram.AllowedUsers = []string{"42"}
	cfg.ApplyDefaults()

	reg := function.NewRegistry()
	require.NoError(t, reg.LoadDirectory(filepath.Join("..", "..", "examples", "functions")))

	var gotCommand string
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, spec executor.Spec) (executor.Result, error) {
			gotCommand = spec.Command
			return executor.Result{ExitCode: 0, Stdout: "ok"}, nil
		},
	}
	app := bot.NewApp(cfg, reg, fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)
	return app, b, &gotCommand
}

func TestInlineParamsCurlURLIntegration(t *testing.T) {
	cfg := &config.Config{Menu: []config.ButtonNode{{
		Name: "Check API", Type: "button", Function: "curl-url",
		Params: map[string]string{"url": "https://example.com/health"},
	}}}
	_, b, command := newParamIntegrationBot(t, cfg)
	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Check API"))
	require.Equal(t, "curl -fsSL --max-time 30 https://example.com/health", *command)
}

func TestInlineParamsJournalUnitIntegration(t *testing.T) {
	cfg, err := config.Parse([]byte(`
telegram:
  bot_token: token
  allowed_users: ["42"]
menu:
  - name: Logs
    type: button
    function: journal-unit
    unit: nginx.service
    lines: 100
`))
	require.NoError(t, err)
	_, b, command := newParamIntegrationBot(t, cfg)
	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Logs"))
	require.Equal(t, "journalctl -u nginx.service -n 100 --no-pager", *command)
}

func TestInlineParamsPingHostDefaultIntegration(t *testing.T) {
	cfg := &config.Config{Menu: []config.ButtonNode{{
		Name: "Ping", Type: "button", Function: "ping-host",
		Params: map[string]string{"host": "example.com"},
	}}}
	_, b, command := newParamIntegrationBot(t, cfg)
	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Ping"))
	require.Equal(t, "ping -c 4 example.com", *command)
}

func TestInlineParamsNestedCategoryIntegration(t *testing.T) {
	cfg := &config.Config{Menu: []config.ButtonNode{{
		Name: "Network", Type: "category", Items: []config.ButtonNode{{
			Name: "Ping", Type: "button", Function: "ping-host",
			Params: map[string]string{"host": "localhost", "count": "2"},
		}},
	}}}
	app, b, command := newParamIntegrationBot(t, cfg)
	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, callbackUpdate(42, "o:"+app.Index.Roots[0]))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Ping"))
	require.Equal(t, "ping -c 2 localhost", *command)
}

func TestInlineParamsConfirmIntegration(t *testing.T) {
	cfg := &config.Config{Menu: []config.ButtonNode{{
		Name: "Check API", Type: "button", Function: "curl-url", Confirm: true,
		Params: map[string]string{"url": "https://example.com/private?x=1&y=2"},
	}}}
	_, b, command := newParamIntegrationBot(t, cfg)
	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Check API"))
	require.Empty(t, *command)
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "✅ Yes"))
	require.Equal(t, "curl -fsSL --max-time 30 https://example.com/private?x=1&y=2", *command)
}

func TestAutoOutputSendsDocumentWhenTooManyChunks(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var stdout strings.Builder
	line := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 4)
	for i := 0; i < 400; i++ {
		stdout.WriteString(line)
		stdout.WriteByte('\n')
	}

	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{ExitCode: 0, Stdout: stdout.String()}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Echo"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Len(t, srv.documents, 1, "auto mode should send one document for long output")
	var preMessages int
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "<pre>") {
			preMessages++
		}
	}
	require.Zero(t, preMessages, "chunked text result should not be sent when document succeeds")
	doc := srv.documents[0]
	require.Contains(t, fmt.Sprintf("%v", doc["filename"]), ".txt")
	require.Contains(t, fmt.Sprintf("%v", doc["caption"]), "Button: Echo")
	rm := fmt.Sprintf("%v", doc["reply_markup"])
	require.Contains(t, rm, "Home")
}

func TestButtonOutputTextKeepsChunkedMessages(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var stdout strings.Builder
	line := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 4)
	for i := 0; i < 400; i++ {
		stdout.WriteString(line)
		stdout.WriteByte('\n')
	}

	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{ExitCode: 0, Stdout: stdout.String()}, nil
		},
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi", Output: "text"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Echo"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.documents)
	var preMessages int
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "<pre>") {
			preMessages++
		}
	}
	require.Greater(t, preMessages, 1)
}

func TestButtonOutputRawSendsUnwrappedHTML(t *testing.T) {
	fake := &executor.FakeExecutor{
		Fn: func(_ context.Context, _ executor.Spec) (executor.Result, error) {
			return executor.Result{
				ExitCode: 0,
				Stdout:   "📊 <b>System Resources</b>\n🧠 CPU: 5%",
			}, nil
		},
	}
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Resources", Type: "button", Function: "command", Command: "res", Output: "raw"},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), fake, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "Resources"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.documents)

	found := false
	for _, m := range srv.messages {
		text, _ := m["text"].(string)
		if strings.Contains(text, "📊 <b>System Resources</b>") {
			found = true
			require.NotContains(t, text, "<pre>")
			require.NotContains(t, text, "Button: Resources")
			require.NotContains(t, text, "--- stdout ---")
		}
	}
	require.True(t, found, "expected raw HTML message without <pre> or command headers")
}

func TestDeliveryRetryTransientThenSucceeds(t *testing.T) {
	srv := &apiServer{failSendN: 2, failSendCode: 500}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	tinyDeliveryRetry(cfg)
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, visibleTexts(srv), "Menu")
	require.GreaterOrEqual(t, srv.sendAttempts, 3, "expected retries before success")
}

func TestDeliveryRetryRateLimitThenSucceeds(t *testing.T) {
	srv := &apiServer{failSendN: 1, failSendCode: 429, failRetryAfter: 0}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	tinyDeliveryRetry(cfg)
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, visibleTexts(srv), "Menu")
	require.Equal(t, 2, srv.sendAttempts)
}

func TestDeliveryRetryPermanentErrorNoRetry(t *testing.T) {
	srv := &apiServer{failSendN: 5, failSendCode: 403}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{Name: "Echo", Type: "button", Function: "command", Command: "echo hi"},
		},
	}
	tinyDeliveryRetry(cfg)
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	b.ProcessUpdate(context.Background(), startCommandUpdate(42, "admin", "/start"))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Empty(t, srv.messages)
	require.Equal(t, 1, srv.sendAttempts, "permanent errors must not retry")
	require.Equal(t, 4, srv.failSendN, "only one failure should have been consumed")
}

func TestTreeExplorerIntegration(t *testing.T) {
	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "subfolder")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "inner.txt"), []byte("inner"), 0o644))

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name:     "tg-tree",
				Type:     "button",
				Function: "tg-tree",
				Path:     tempDir,
			},
		},
	}
	cfg.ApplyDefaults()

	reg := function.NewRegistry()
	app := bot.NewApp(cfg, reg, &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()

	// 1. Open menu and tap tg-tree button
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "tg-tree"))

	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srvDeletes := srv.deletes
	srv.mu.Unlock()

	require.Equal(t, 0, srvDeletes, "nav message should not be deleted so keyboard remains")

	joined := strings.Join(msgs, "\n")
	require.Contains(t, joined, "Folder Explorer")
	require.Contains(t, joined, "subfolder")
	require.NotContains(t, joined, `type="copy_text"`)
	require.NotContains(t, joined, "🔄 Refresh")
	require.NotContains(t, joined, "❌ Close")

	tokSub := app.TreeState.GetToken(subDir)

	// Subfolder is closed by default: inner.txt must not appear yet
	require.NotContains(t, joined, "inner.txt")

	// 2. Click in-message folder button for subfolder to open it
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokSub))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()

	innerPath := filepath.Join(subDir, "inner.txt")
	tokInner := app.TreeState.GetToken(innerPath)

	joined = strings.Join(msgs, "\n")
	require.Contains(t, joined, "inner.txt")
	require.Contains(t, joined, fmt.Sprintf(`<tg-button type="callback_data" data="t:file:%s">inner.txt</tg-button>`, tokInner))
	require.Contains(t, joined, ">subfolder/</tg-button> 👈<br/>")
	require.Contains(t, joined, "<hr/>")
	require.Contains(t, joined, "<p><code>~/subfolder</code></p>")
	require.Contains(t, joined, fmt.Sprintf(`<tg-button type="copy_text" style="success" text="%s">copy full path</tg-button>`, subDir))
	require.Contains(t, joined, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:zip:%s">download folder</tg-button>`, tokSub))
	require.NotContains(t, joined, "🏠 Home")

	// 2b. Click file button: edits tree message in place to show copy path and get file buttons
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:file:"+tokInner))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	require.Empty(t, srv.documents, "document should not be sent directly on file click")
	srv.mu.Unlock()

	lastMsg := msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "inner.txt")
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" data="t:file:%s">inner.txt</tg-button> 👈<br/>`, tokInner))
	require.NotContains(t, lastMsg, ">subfolder/</tg-button> 👈")
	require.Contains(t, lastMsg, "<hr/>")
	require.Contains(t, lastMsg, "<p><code>~/subfolder/inner.txt</code></p>")
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="copy_text" style="success" text="%s">copy full path</tg-button>`, innerPath))
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:send:%s">download file</tg-button>`, tokInner))
	require.Contains(t, lastMsg, "<p>file operations:</p>")
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:ren:%s">rename</tg-button>`, tokInner))
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:edit:%s">edit</tg-button>`, tokInner))

	// 2c. Click get file button to send file as document
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:send:"+tokInner))

	srv.mu.Lock()
	require.NotEmpty(t, srv.documents)
	lastDoc := srv.documents[len(srv.documents)-1]
	srv.mu.Unlock()
	require.Equal(t, "inner.txt", lastDoc["filename"])
	require.Equal(t, len("inner"), lastDoc["size"])

	// 2d. Click get folder button to send zipped folder as document
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:zip:"+tokSub))

	srv.mu.Lock()
	lastDoc = srv.documents[len(srv.documents)-1]
	srv.mu.Unlock()
	require.Equal(t, "subfolder.zip", lastDoc["filename"])
	require.Greater(t, lastDoc["size"].(int), 0)

	// 2e. Test empty file (0 bytes) alert when Send File is clicked
	emptyPath := filepath.Join(subDir, "empty.txt")
	require.NoError(t, os.WriteFile(emptyPath, []byte(""), 0o644))
	tokEmpty := app.TreeState.GetToken(emptyPath)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:send:"+tokEmpty))

	srv.mu.Lock()
	lastAns := srv.callbackAnswers[len(srv.callbackAnswers)-1]
	srv.mu.Unlock()
	require.Contains(t, lastAns["text"], "empty")

	// 2f. Test large file (> 20 MB) alert when Send File is clicked
	largePath := filepath.Join(subDir, "large.bin")
	fLarge, err := os.Create(largePath)
	require.NoError(t, err)
	require.NoError(t, fLarge.Truncate(25*1024*1024))
	_ = fLarge.Close()
	tokLarge := app.TreeState.GetToken(largePath)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:send:"+tokLarge))

	srv.mu.Lock()
	lastAns = srv.callbackAnswers[len(srv.callbackAnswers)-1]
	srv.mu.Unlock()
	require.Contains(t, lastAns["text"], "too large")

	// 3. Click the same folder button again to close it
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokSub))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()

	lastMsg = msgs[len(msgs)-1]
	require.NotContains(t, lastMsg, "inner.txt")
	require.Contains(t, lastMsg, ">subfolder</tg-button> 👈<br/>")
	require.NotContains(t, lastMsg, "inner.txt</tg-button> 👈")
	require.Contains(t, lastMsg, "<hr/>")
	require.Contains(t, lastMsg, "<p><code>~/subfolder</code></p>")
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="copy_text" style="success" text="%s">copy full path</tg-button>`, subDir))
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:zip:%s">download folder</tg-button>`, tokSub))

	// 4. Click Close callback
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:close"))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()

	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "closed")
}

func TestTreeEditAndRenameIntegration(t *testing.T) {
	tempDir := t.TempDir()
	docPath := filepath.Join(tempDir, "doc.txt")
	require.NoError(t, os.WriteFile(docPath, []byte("initial content"), 0o644))

	srv := &apiServer{}
	server := httptest.NewServer(srv)
	defer server.Close()

	cfg := &config.Config{
		WorkDir: tempDir,
		Telegram: config.TelegramConfig{
			BotToken:     "TEST:TOKEN",
			API:          server.URL,
			AllowedUsers: []string{"42", "admin"},
		},
		Menu: []config.ButtonNode{
			{Name: "tg-tree", Type: "button", Function: "tg-tree", Path: tempDir},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	app.SetHTTPClient(server.Client())
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, server.Client()),
		tgbot.WithServerURL(server.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()

	// 1. Open tree explorer
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "tg-tree"))

	// 2. Select doc.txt
	tokDoc := app.TreeState.GetToken(docPath)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:file:"+tokDoc))

	// 3. Click Edit File
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokDoc))

	srv.mu.Lock()
	msgs := visibleTexts(srv)
	promptMsg := srv.messages[len(srv.messages)-1]
	promptMsgID := promptMsg["message_id"].(int)
	srv.mu.Unlock()

	lastMsg := msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "replace 'doc.txt'")
	require.Contains(t, lastMsg, "doc.txt")

	// 3. Send non-reply text - should update content without requiring a reply
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "new text content"))
	content, err := os.ReadFile(docPath)
	require.NoError(t, err)
	require.Equal(t, "new text content", string(content))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	notifMsg := srv.messages[len(srv.messages)-2]
	require.NotEmpty(t, notifMsg["reply_markup"])
	require.Contains(t, fmt.Sprintf("%v", notifMsg["reply_markup"]), "tg-tree")
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-2], "content updated successfully")
	require.Contains(t, msgs[len(msgs)-1], "Folder Explorer")
	require.Contains(t, msgs[len(msgs)-1], "doc.txt")

	// 4. Click Edit File again and reply with wrong format (.png vs .txt)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokDoc))
	srv.mu.Lock()
	promptMsgID = srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyDocumentUpdate(42, "admin", "image.png", "file123", promptMsgID))
	content, err = os.ReadFile(docPath)
	require.NoError(t, err)
	require.Equal(t, "new text content", string(content))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-1], "Format mismatch")

	// 5. Click Edit File again and reply with same format document (uploaded.txt)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokDoc))
	srv.mu.Lock()
	promptMsgID = srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyDocumentUpdate(42, "admin", "uploaded.txt", "file456", promptMsgID))
	content, err = os.ReadFile(docPath)
	require.NoError(t, err)
	require.Equal(t, "mock-file-content", string(content))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "✅ File content replaced.")
	require.Contains(t, lastMsg, "\n\n<p>Rename the file too?</p>")
	require.Contains(t, lastMsg, fmt.Sprintf("<code>Old: </code><code>%s</code> 👇", html.EscapeString(filepath.Base(docPath))))
	require.Contains(t, lastMsg, "<code>New: </code><code>uploaded.txt</code>")
	require.Contains(t, lastMsg, ">Yes</tg-button>")
	require.Contains(t, lastMsg, ">No</tg-button>")
	require.NotContains(t, lastMsg, "✅ Yes")
	require.NotContains(t, lastMsg, "❌ No")
	require.Contains(t, lastMsg, "t:rfe:y:")
	require.Contains(t, lastMsg, "t:rfe:n:")

	// Extract callback data from the prompt buttons
	uploadedPath := filepath.Join(tempDir, "uploaded.txt")
	tokUploaded := app.TreeState.GetToken(uploadedPath)
	// Click Yes to rename
	b.ProcessUpdate(ctx, callbackUpdate(42, fmt.Sprintf("t:rfe:y:%s:%s", tokDoc, tokUploaded)))
	require.FileExists(t, uploadedPath)
	require.NoFileExists(t, docPath)

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-1], "Folder Explorer")
	require.Contains(t, msgs[len(msgs)-1], "uploaded.txt")

	// 6. Rename uploaded.txt without format ("newname") without reply
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:ren:"+tokUploaded))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-1], "Send the new name for 'uploaded.txt'")

	b.ProcessUpdate(ctx, textUpdate(42, "admin", "newname"))
	// File should be renamed to newname.txt (with same format)
	newnameWithExt := filepath.Join(tempDir, "newname.txt")
	require.FileExists(t, newnameWithExt)
	require.NoFileExists(t, uploadedPath)

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "File got renamed to <code>newname.txt</code> with the same format")
	require.Contains(t, lastMsg, "Do you wish to delete the format as well")
	require.Contains(t, lastMsg, "t:rfd:y:")
	require.Contains(t, lastMsg, "t:rfd:n:")

	// Click Yes (delete format)
	newnameNoExt := filepath.Join(tempDir, "newname")
	tokWithExt := app.TreeState.GetToken(newnameWithExt)
	tokNoExt := app.TreeState.GetToken(newnameNoExt)
	b.ProcessUpdate(ctx, callbackUpdate(42, fmt.Sprintf("t:rfd:y:%s:%s", tokWithExt, tokNoExt)))
	require.FileExists(t, newnameNoExt)
	require.NoFileExists(t, newnameWithExt)

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-1], "Folder Explorer")
	require.Contains(t, msgs[len(msgs)-1], "newname")

	// 7. Rename newname with format ("final.md")
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:ren:"+tokNoExt))
	srv.mu.Lock()
	promptMsgID = srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyTextUpdate(42, "admin", "final.md", promptMsgID))
	finalPath := filepath.Join(tempDir, "final.md")
	require.FileExists(t, finalPath)
	require.NoFileExists(t, newnameNoExt)

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	notifMsg = srv.messages[len(srv.messages)-2]
	require.NotEmpty(t, notifMsg["reply_markup"])
	require.Contains(t, fmt.Sprintf("%v", notifMsg["reply_markup"]), "tg-tree")
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-2], "File renamed to <code>final.md</code>")
	require.Contains(t, msgs[len(msgs)-1], "Folder Explorer")
	require.Contains(t, msgs[len(msgs)-1], "final.md")
}

func TestTreeEditAndRename_NoAndCancel(t *testing.T) {
	tempDir := t.TempDir()
	docPath := filepath.Join(tempDir, "sample.txt")
	require.NoError(t, os.WriteFile(docPath, []byte("initial"), 0o644))

	srv := &apiServer{}
	server := httptest.NewServer(srv)
	defer server.Close()

	cfg := &config.Config{
		WorkDir: tempDir,
		Telegram: config.TelegramConfig{
			BotToken:     "TEST:TOKEN",
			API:          server.URL,
			AllowedUsers: []string{"42", "admin"},
		},
		Menu: []config.ButtonNode{
			{Name: "tg-tree", Type: "button", Function: "tg-tree", Path: tempDir},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	app.SetHTTPClient(server.Client())
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, server.Client()),
		tgbot.WithServerURL(server.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()

	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "tg-tree"))

	tokDoc := app.TreeState.GetToken(docPath)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:file:"+tokDoc))

	// 1. Test /cancel while awaiting edit
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokDoc))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "/cancel"))
	_, ok := app.TreeState.GetAwaiting(42)
	require.False(t, ok)

	// 2. Test edit with file then clicking "No" on rename
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokDoc))
	srv.mu.Lock()
	promptMsgID := srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyDocumentUpdate(42, "admin", "other.txt", "file789", promptMsgID))
	content, err := os.ReadFile(docPath)
	require.NoError(t, err)
	require.Equal(t, "mock-file-content", string(content))

	// Click No to keep original name
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:rfe:n:"+tokDoc))
	require.FileExists(t, docPath)
	require.NoFileExists(t, filepath.Join(tempDir, "other.txt"))

	// 3. Test rename without format then clicking "No" (Keep format)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:ren:"+tokDoc))
	srv.mu.Lock()
	promptMsgID = srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyTextUpdate(42, "admin", "keptname", promptMsgID))
	keptWithExt := filepath.Join(tempDir, "keptname.txt")
	require.FileExists(t, keptWithExt)
	require.NoFileExists(t, docPath)

	tokKeptWithExt := app.TreeState.GetToken(keptWithExt)
	// Click No (keep format)
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:rfd:n:"+tokKeptWithExt))
	require.FileExists(t, keptWithExt)
	require.NoFileExists(t, filepath.Join(tempDir, "keptname"))

	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-1], "Folder Explorer")
	require.Contains(t, msgs[len(msgs)-1], "keptname.txt")
}

func TestTreeEditAndRename_FolderOpenPreserved(t *testing.T) {
	tempDir := t.TempDir()
	subADir := filepath.Join(tempDir, "subA")
	subBDir := filepath.Join(subADir, "subB")
	require.NoError(t, os.MkdirAll(subBDir, 0o755))
	itemPath := filepath.Join(subBDir, "item.txt")
	require.NoError(t, os.WriteFile(itemPath, []byte("initial-data"), 0o644))

	srv := &apiServer{}
	server := httptest.NewServer(srv)
	defer server.Close()

	cfg := &config.Config{
		WorkDir: tempDir,
		Telegram: config.TelegramConfig{
			BotToken:     "TEST:TOKEN",
			API:          server.URL,
			AllowedUsers: []string{"42", "admin"},
		},
		Menu: []config.ButtonNode{
			{Name: "tg-tree", Type: "button", Function: "tg-tree", Path: tempDir},
		},
	}
	cfg.ApplyDefaults()

	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	app.SetHTTPClient(server.Client())
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, server.Client()),
		tgbot.WithServerURL(server.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	ctx := context.Background()

	// 1. Open tree explorer
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "tg-tree"))

	tokSubA := app.TreeState.GetToken(subADir)
	tokSubB := app.TreeState.GetToken(subBDir)

	// 2. Expand subA
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokSubA))
	// 3. Expand subB
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokSubB))

	tokItem := app.TreeState.GetToken(itemPath)

	// 4. Select item.txt
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:file:"+tokItem))

	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srv.mu.Unlock()
	lastTree := msgs[len(msgs)-1]
	require.Contains(t, lastTree, "subA/")
	require.Contains(t, lastTree, "subB/")
	require.Contains(t, lastTree, "item.txt")
	require.Contains(t, lastTree, "<p>file operations:</p>")
	require.Contains(t, lastTree, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:ren:%s">rename</tg-button>`, tokItem))
	require.Contains(t, lastTree, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:edit:%s">edit</tg-button>`, tokItem))

	// 5. Edit file item.txt
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:edit:"+tokItem))
	srv.mu.Lock()
	promptMsgID := srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyTextUpdate(42, "admin", "modified-content", promptMsgID))
	content, err := os.ReadFile(itemPath)
	require.NoError(t, err)
	require.Equal(t, "modified-content", string(content))

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	// The message before last is the status, and the last message is the newly sent updated tree
	require.Contains(t, msgs[len(msgs)-2], "content updated successfully")
	updatedTree := msgs[len(msgs)-1]
	require.Contains(t, updatedTree, "Folder Explorer")
	// Verify subA and subB are STILL expanded!
	require.Contains(t, updatedTree, "subA/")
	require.Contains(t, updatedTree, "subB/")
	require.Contains(t, updatedTree, "item.txt")
	require.Contains(t, updatedTree, fmt.Sprintf(`<tg-button type="callback_data" data="t:file:%s">item.txt</tg-button> 👈<br/>`, tokItem))

	// 6. Rename item.txt to renamed.txt
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:ren:"+tokItem))
	srv.mu.Lock()
	promptMsgID = srv.messages[len(srv.messages)-1]["message_id"].(int)
	srv.mu.Unlock()

	b.ProcessUpdate(ctx, replyTextUpdate(42, "admin", "renamed.txt", promptMsgID))
	renamedPath := filepath.Join(subBDir, "renamed.txt")
	require.FileExists(t, renamedPath)
	require.NoFileExists(t, itemPath)

	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	require.Contains(t, msgs[len(msgs)-2], "File renamed to <code>renamed.txt</code>")
	updatedTreeAfterRename := msgs[len(msgs)-1]
	require.Contains(t, updatedTreeAfterRename, "Folder Explorer")
	// Verify both subA and subB remain open!
	require.Contains(t, updatedTreeAfterRename, "subA/")
	require.Contains(t, updatedTreeAfterRename, "subB/")
	require.Contains(t, updatedTreeAfterRename, "renamed.txt")
	tokRenamed := app.TreeState.GetToken(renamedPath)
	require.Contains(t, updatedTreeAfterRename, fmt.Sprintf(`<tg-button type="callback_data" data="t:file:%s">renamed.txt</tg-button> 👈<br/>`, tokRenamed))
}

func TestTreeEditMessageText_FallbackWhenUneditable(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "folder")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	filePath := filepath.Join(subDir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0o644))

	srv := &apiServer{}
	server := httptest.NewServer(srv)
	defer server.Close()

	cfg := &config.Config{
		WorkDir: tempDir,
		Telegram: config.TelegramConfig{
			BotToken:     "TEST:TOKEN",
			API:          server.URL,
			AllowedUsers: []string{"42", "admin"},
		},
		Menu: []config.ButtonNode{
			{Name: "tg-tree", Type: "button", Function: "tg-tree", Path: tempDir},
		},
	}
	cfg.ApplyDefaults()
	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, server.Client()),
		tgbot.WithServerURL(server.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)
	ctx := context.Background()

	// 1. Open tree
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "🌳 tg-tree"))
	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srv.mu.Unlock()
	require.NotEmpty(t, msgs)

	tokFolder := app.TreeState.GetToken(subDir)
	tokFile := app.TreeState.GetToken(filePath)

	// Expand folder
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokFolder))

	// Now simulate that Telegram returns "Bad Request: message can't be edited" for the current message.
	srv.mu.Lock()
	srv.failEditMessageWithUneditable = true
	deletesBefore := srv.deletes
	messagesBefore := len(srv.messages)
	srv.mu.Unlock()

	// 2. Click file to select it.
	// Because editMessageText fails with "message can't be edited",
	// the fallback should delete the old uneditable message and send a fresh tree message.
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:file:"+tokFile))

	srv.mu.Lock()
	deletesAfter := srv.deletes
	messagesAfter := len(srv.messages)
	msgs = visibleTexts(srv)
	srv.mu.Unlock()

	require.Greater(t, deletesAfter, deletesBefore, "stale uneditable message should be deleted")
	require.Greater(t, messagesAfter, messagesBefore, "new tree message should be sent")
	lastMsg := msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, "hello.txt")
	require.Contains(t, lastMsg, "file operations:")
}

func TestTreeOpenOnUncaughtPathMessage(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "subfolder")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	fileInSub := filepath.Join(subDir, "inner.txt")
	require.NoError(t, os.WriteFile(fileInSub, []byte("hello"), 0o644))

	srv := &apiServer{}
	server := httptest.NewServer(srv)
	defer server.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			BotToken:     "test-token",
			API:          server.URL,
			AllowedUsers: []string{"42", "admin"},
		},
		WorkDir: tempDir,
		Menu: []config.ButtonNode{
			{Name: "tg-tree", Type: "button", Function: "tg-tree", Path: tempDir},
		},
	}
	cfg.ApplyDefaults()
	app := bot.NewApp(cfg, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, server.Client()),
		tgbot.WithServerURL(server.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)
	ctx := context.Background()

	// 1. Send uncaught directory path message (absolute path)
	b.ProcessUpdate(ctx, textUpdate(42, "admin", subDir))
	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srv.mu.Unlock()
	require.NotEmpty(t, msgs)
	lastMsg := msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, ">subfolder/</tg-button> 👈<br/>")
	require.Contains(t, lastMsg, "inner.txt")
	require.Contains(t, lastMsg, "download folder")
	require.Contains(t, lastMsg, "<hr/>")
	require.Contains(t, lastMsg, `<tg-button type="callback_data" data="t:home">back</tg-button>`)

	// 1b. Clicking back returns to default directory
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:home"))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.NotContains(t, lastMsg, `data="t:home">back</tg-button>`)

	// 2. Send uncaught file path message: opens parent dir as root and selects file
	b.ProcessUpdate(ctx, textUpdate(42, "admin", fileInSub))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, ">subfolder/</tg-button><br/>")
	require.Contains(t, lastMsg, ">inner.txt</tg-button> 👈<br/>")
	require.Contains(t, lastMsg, "file operations:")
	require.Contains(t, lastMsg, "download file")
	require.Contains(t, lastMsg, "rename")
	require.Contains(t, lastMsg, "edit")
	require.Contains(t, lastMsg, `<tg-button type="callback_data" data="t:home">back</tg-button>`)

	// 2b. Send path with quotes/backticks
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "`"+subDir+"`"))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, "inner.txt")

	// 2c. Send path with cd prefix
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "cd "+subDir))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, "inner.txt")

	// 2d. Send relative path 'inner.txt' while browsing subfolder
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "inner.txt"))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, "inner.txt")
	require.Contains(t, lastMsg, "file operations:")

	// 2e. Send '..' to navigate back to parent folder
	b.ProcessUpdate(ctx, textUpdate(42, "admin", ".."))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Folder Explorer")
	require.Contains(t, lastMsg, "subfolder")

	// 3. Send non-existent explicit path: shows "Path does not exist"
	missingPath := filepath.Join(tempDir, "missing", "dir")
	b.ProcessUpdate(ctx, textUpdate(42, "admin", missingPath))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Path does not exist:")

	// 4. Send unknown command: shows "Use /start"
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "/unknown_cmd"))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Use /start to open the menu.")

	// 5. Send random unknown text: shows "Unknown option"
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "some unknown text"))
	srv.mu.Lock()
	msgs = visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg = msgs[len(msgs)-1]
	require.Contains(t, lastMsg, "Unknown option. Use /start to open the menu.")
}

func TestTreeAdvDlIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	tempDir := t.TempDir()
	gitRepoDir := filepath.Join(tempDir, "my-git-app")
	require.NoError(t, os.Mkdir(gitRepoDir, 0o755))

	cmdInit := exec.Command("git", "init")
	cmdInit.Dir = gitRepoDir
	require.NoError(t, cmdInit.Run())

	cmdUser := exec.Command("git", "config", "user.name", "Tester")
	cmdUser.Dir = gitRepoDir
	require.NoError(t, cmdUser.Run())
	cmdEmail := exec.Command("git", "config", "user.email", "tester@example.com")
	cmdEmail.Dir = gitRepoDir
	require.NoError(t, cmdEmail.Run())

	require.NoError(t, os.WriteFile(filepath.Join(gitRepoDir, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitRepoDir, ".gitignore"), []byte("ignored.log\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitRepoDir, "ignored.log"), []byte("secret log"), 0o644))

	cmdAdd := exec.Command("git", "add", "main.go", ".gitignore")
	cmdAdd.Dir = gitRepoDir
	require.NoError(t, cmdAdd.Run())

	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = gitRepoDir
	require.NoError(t, cmdCommit.Run())

	regularDir := filepath.Join(tempDir, "regular-folder")
	require.NoError(t, os.Mkdir(regularDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(regularDir, "file.txt"), []byte("content"), 0o644))

	srv := &apiServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			API:          ts.URL,
			BotToken:     "123:ABC",
			AllowedUsers: []string{"42"},
		},
		Menu: []config.ButtonNode{
			{
				Name:     "tg-tree",
				Type:     "button",
				Function: "tg-tree",
				Path:     tempDir,
			},
		},
	}
	cfg.ApplyDefaults()

	reg := function.NewRegistry()
	app := bot.NewApp(cfg, reg, &executor.FakeExecutor{}, nil)
	b, err := app.NewBotWithOptions(
		tgbot.WithHTTPClient(5*time.Second, ts.Client()),
		tgbot.WithServerURL(ts.URL),
		tgbot.WithSkipGetMe(),
		tgbot.WithNotAsyncHandlers(),
	)
	require.NoError(t, err)

	// 1. Start tree explorer
	b.ProcessUpdate(ctx, startCommandUpdate(42, "admin", "/start"))
	b.ProcessUpdate(ctx, textUpdate(42, "admin", "tg-tree"))

	tokGit := app.TreeState.GetToken(gitRepoDir)
	tokReg := app.TreeState.GetToken(regularDir)

	// 2. Select git repo folder
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:tog:"+tokGit))

	srv.mu.Lock()
	msgs := visibleTexts(srv)
	srv.mu.Unlock()
	lastMsg := msgs[len(msgs)-1]

	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:zip:%s">download folder</tg-button>`, tokGit))
	require.Contains(t, lastMsg, fmt.Sprintf(`<tg-button type="callback_data" style="success" data="t:gzip:%s">adv dl</tg-button>`, tokGit))

	// 3. Click adv dl button on git folder
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:gzip:"+tokGit))

	srv.mu.Lock()
	require.NotEmpty(t, srv.documents)
	lastDoc := srv.documents[len(srv.documents)-1]
	srv.mu.Unlock()

	require.Equal(t, "my-git-app.zip", lastDoc["filename"])
	require.Greater(t, lastDoc["size"].(int), 0)

	// 4. Test clicking adv dl on folder without git
	b.ProcessUpdate(ctx, callbackUpdate(42, "t:gzip:"+tokReg))
	srv.mu.Lock()
	lastAns := srv.callbackAnswers[len(srv.callbackAnswers)-1]
	srv.mu.Unlock()
	require.Contains(t, lastAns["text"], "not a git repository")
}
