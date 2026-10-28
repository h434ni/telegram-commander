package bot

import (
	"os"
	"path/filepath"
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

	// User state tracking
	mgr.SetUserPath(42, p1)
	require.Equal(t, p1, mgr.GetUserPath(42))

	mgr.SetUserMessage(42, 999)
	require.Equal(t, 999, mgr.GetUserMessageID(42))

	mgr.ClearUserMessage(42)
	require.Equal(t, 0, mgr.GetUserMessageID(42))
}

func TestBuildTreeRichMessageHTML(t *testing.T) {
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
	html, err := app.buildTreeRichMessageHTML(tempDir)
	require.NoError(t, err)

	require.Contains(t, html, "Folder Explorer")
	require.Contains(t, html, tempDir)
	require.Contains(t, html, "📁 <b>alpha</b>")
	require.Contains(t, html, "📁 <b>beta</b>")
	require.Contains(t, html, "📁 nested1")
	require.Contains(t, html, "📄 file1.txt")
	require.Contains(t, html, "3 directories, 2 files")

	// Rich text <tg-button> tags
	tokAlpha := app.TreeState.GetToken(sub2)
	tokBeta := app.TreeState.GetToken(sub1)
	require.Contains(t, html, `<tg-button type="callback_data" style="primary" data="t:cd:`+tokAlpha+`">Open</tg-button>`)
	require.Contains(t, html, `<tg-button type="callback_data" style="primary" data="t:cd:`+tokBeta+`">Open</tg-button>`)

	// Navigation buttons in <tg-button-row>
	require.Contains(t, html, "<tg-button-row")
	require.Contains(t, html, ">⬆️ Up</tg-button>")
	require.Contains(t, html, ">🏠 Home</tg-button>")
	require.Contains(t, html, ">🔄 Refresh</tg-button>")
	require.Contains(t, html, ">❌ Close</tg-button>")
}

func TestBuildTreeRichMessageHTMLEmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	html, err := app.buildTreeRichMessageHTML(tempDir)
	require.NoError(t, err)
	require.Contains(t, html, "(empty directory)")
	require.Contains(t, html, "0 directories, 0 files")
}
