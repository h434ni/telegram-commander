package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azolfagharj/telegram-commander/internal/config"
	"github.com/azolfagharj/telegram-commander/internal/executor"
	"github.com/azolfagharj/telegram-commander/internal/function"
)

func TestTreeStateManager(t *testing.T) {
	home := t.TempDir()
	mgr := NewTreeStateManager(home)
	require.Equal(t, home, mgr.Home())

	p1 := filepath.Join(home, "apps")
	tok1 := mgr.GetToken(p1)
	require.NotEmpty(t, tok1)

	// Reusing token for same path
	tok2 := mgr.GetToken(p1)
	require.Equal(t, tok1, tok2)

	// Path lookup
	gotPath := mgr.GetPath(tok1)
	require.Equal(t, p1, gotPath)

	// Different path gets different token
	p2 := filepath.Join(home, "scripts")
	tok3 := mgr.GetToken(p2)
	require.NotEmpty(t, tok3)
	require.NotEqual(t, tok1, tok3)
	require.Equal(t, p2, mgr.GetPath(tok3))
}

func TestBuildTreeMessage(t *testing.T) {
	tempDir := t.TempDir()

	// Create subdirectories and files
	sub1 := filepath.Join(tempDir, "beta")
	sub2 := filepath.Join(tempDir, "alpha")
	subHidden := filepath.Join(tempDir, ".config")
	require.NoError(t, os.Mkdir(sub1, 0o755))
	require.NoError(t, os.Mkdir(sub2, 0o755))
	require.NoError(t, os.Mkdir(subHidden, 0o755))

	// Nested subdirectories
	require.NoError(t, os.Mkdir(filepath.Join(sub1, "nested1"), 0o755))

	// Files
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file1.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file2.log"), []byte("log"), 0o644))

	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	text, markup, err := app.buildTreeMessage(tempDir, 0)
	require.NoError(t, err)

	require.Contains(t, text, "Folder Explorer")
	require.Contains(t, text, tempDir)
	require.Contains(t, text, "📁 alpha")
	require.Contains(t, text, "📁 beta")
	require.Contains(t, text, "📁 nested1")
	require.Contains(t, text, "📄 file1.txt")
	require.Contains(t, text, "3 directories, 2 files")

	require.NotNil(t, markup)
	require.NotEmpty(t, markup.InlineKeyboard)

	// First row of folder buttons: non-hidden first alphabetically (alpha, beta)
	row0 := markup.InlineKeyboard[0]
	require.Len(t, row0, 2)
	require.Equal(t, "📁 alpha", row0[0].Text)
	require.True(t, strings.HasPrefix(row0[0].CallbackData, cbTreeCD))
	require.Equal(t, "📁 beta", row0[1].Text)
	require.True(t, strings.HasPrefix(row0[1].CallbackData, cbTreeCD))

	// Navigation controls row (last row)
	lastRow := markup.InlineKeyboard[len(markup.InlineKeyboard)-1]
	ctrlTexts := make([]string, len(lastRow))
	for i, btn := range lastRow {
		ctrlTexts[i] = btn.Text
	}
	require.Contains(t, ctrlTexts, "⬆️ Up")
	require.Contains(t, ctrlTexts, "🏠 Home")
	require.Contains(t, ctrlTexts, "🔄 Refresh")
	require.Contains(t, ctrlTexts, "❌ Close")
}

func TestBuildTreeMessagePagination(t *testing.T) {
	tempDir := t.TempDir()

	// Create 10 subdirectories to exceed default treePageSize (8)
	for i := 1; i <= 10; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tempDir, fmt.Sprintf("dir_%02d", i)), 0o755))
	}

	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)

	// Page 0
	_, markup0, err := app.buildTreeMessage(tempDir, 0)
	require.NoError(t, err)

	// Find pagination row
	var pageRow0 []string
	for _, row := range markup0.InlineKeyboard {
		for _, b := range row {
			if strings.Contains(b.Text, "1/2") || strings.Contains(b.Text, "Next") {
				for _, btn := range row {
					pageRow0 = append(pageRow0, btn.Text)
				}
				break
			}
		}
	}
	require.NotEmpty(t, pageRow0)
	require.Contains(t, pageRow0, "1/2")
	require.Contains(t, pageRow0, "Next ▶️")

	// Page 1
	_, markup1, err := app.buildTreeMessage(tempDir, 1)
	require.NoError(t, err)

	var pageRow1 []string
	for _, row := range markup1.InlineKeyboard {
		for _, b := range row {
			if strings.Contains(b.Text, "2/2") || strings.Contains(b.Text, "Prev") {
				for _, btn := range row {
					pageRow1 = append(pageRow1, btn.Text)
				}
				break
			}
		}
	}
	require.NotEmpty(t, pageRow1)
	require.Contains(t, pageRow1, "◀️ Prev")
	require.Contains(t, pageRow1, "2/2")
}
