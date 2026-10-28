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

	// Expand / Collapse tracking
	require.False(t, mgr.IsExpanded(42, p1))
	nowOpen := mgr.ToggleExpanded(42, p1)
	require.True(t, nowOpen)
	require.True(t, mgr.IsExpanded(42, p1))

	nowOpen = mgr.ToggleExpanded(42, p1)
	require.False(t, nowOpen)
	require.False(t, mgr.IsExpanded(42, p1))

	mgr.ToggleExpanded(42, p1)
	mgr.ClearExpanded(42)
	require.False(t, mgr.IsExpanded(42, p1))

	mgr.ClearUserMessage(42)
	require.Equal(t, 0, mgr.GetUserMessageID(42))
}

func TestBuildTreeRichMessageHTML_CollapseByDefaultAndToggle(t *testing.T) {
	tempDir := t.TempDir()

	subBeta := filepath.Join(tempDir, "beta")
	subAlpha := filepath.Join(tempDir, "alpha")
	subHidden := filepath.Join(tempDir, ".config")
	require.NoError(t, os.Mkdir(subBeta, 0o755))
	require.NoError(t, os.Mkdir(subAlpha, 0o755))
	require.NoError(t, os.Mkdir(subHidden, 0o755))

	// Nested subdirectories inside beta
	nestedDir := filepath.Join(subBeta, "nested1")
	require.NoError(t, os.Mkdir(nestedDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "deep.txt"), []byte("deep"), 0o644))

	// Files
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file1.txt"), []byte("hello"), 0o644))

	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)

	// 1. Initial view: subfolders are CLOSED by default
	html1, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	require.Contains(t, html1, "Folder Explorer")
	require.Contains(t, html1, tempDir)

	tokAlpha := app.TreeState.GetToken(subAlpha)
	tokBeta := app.TreeState.GetToken(subBeta)

	// Folder names are the buttons themselves
	require.Contains(t, html1, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokAlpha+`">📁 alpha</tg-button>`)
	require.Contains(t, html1, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokBeta+`">📁 beta</tg-button>`)
	require.Contains(t, html1, "📄 file1.txt")

	// Closed by default: nested1 and deep.txt must NOT appear yet
	require.NotContains(t, html1, "nested1")
	require.NotContains(t, html1, "deep.txt")
	// Dot folder must NOT appear
	require.NotContains(t, html1, ".config")

	// 2. Open parent folder 'beta'
	app.TreeState.ToggleExpanded(42, subBeta)
	html2, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	// Button updates to open icon and success style
	require.Contains(t, html2, `<tg-button type="callback_data" style="success" data="t:tog:`+tokBeta+`">📂 beta</tg-button>`)
	// Nested directory now appears
	require.Contains(t, html2, "📁 nested1")

	// 3. Press parent folder 'beta' again to close it
	app.TreeState.ToggleExpanded(42, subBeta)
	html3, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	// Button collapses back to closed icon and primary style
	require.Contains(t, html3, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokBeta+`">📁 beta</tg-button>`)
	// Nested items are hidden again
	require.NotContains(t, html3, "nested1")
}

func TestBuildTreeRichMessageHTMLEmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	html, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	require.Contains(t, html, "(empty)")
}
