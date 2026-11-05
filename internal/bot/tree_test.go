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

	// Selected folder tracking
	require.Empty(t, mgr.GetSelectedFolder(42))
	mgr.SetSelectedFolder(42, p1)
	require.Equal(t, p1, mgr.GetSelectedFolder(42))

	nowOpen = mgr.ToggleExpanded(42, p1)
	require.False(t, nowOpen)
	require.False(t, mgr.IsExpanded(42, p1))

	mgr.ToggleExpanded(42, p1)
	mgr.ClearExpanded(42)
	require.False(t, mgr.IsExpanded(42, p1))

	mgr.ClearUserMessage(42)
	require.Equal(t, 0, mgr.GetUserMessageID(42))
	require.Empty(t, mgr.GetSelectedFolder(42))
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

	// 1. Initial view: subfolders are CLOSED by default, no individual line copy buttons, no close/refresh buttons
	html1, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	require.Contains(t, html1, "Folder Explorer")
	require.NotContains(t, html1, tempDir)

	tokAlpha := app.TreeState.GetToken(subAlpha)
	tokBeta := app.TreeState.GetToken(subBeta)

	// Folder names are blue (primary) buttons without line-level copy buttons
	require.Contains(t, html1, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokAlpha+`">alpha</tg-button>`)
	require.Contains(t, html1, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokBeta+`">beta</tg-button>`)
	file1Path := filepath.Join(tempDir, "file1.txt")
	tokFile1 := app.TreeState.GetToken(file1Path)
	require.Contains(t, html1, `<tg-button type="callback_data" data="t:file:`+tokFile1+`">file1.txt</tg-button>`)
	require.NotContains(t, html1, `type="copy_text"`)
	require.NotContains(t, html1, "🏠 Home")
	require.NotContains(t, html1, "🔄 Refresh")
	require.NotContains(t, html1, "❌ Close")

	// Closed by default: nested1 and deep.txt must NOT appear yet
	require.NotContains(t, html1, "nested1")
	require.NotContains(t, html1, "deep.txt")
	// Dot folder must NOT appear
	require.NotContains(t, html1, ".config")

	// 2. Open parent folder 'beta' and select it
	app.TreeState.ToggleExpanded(42, subBeta)
	app.TreeState.SetSelectedFolder(42, subBeta)
	html2, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	// Button stays blue (primary) but has a trailing slash and 👈 when selected/open
	require.Contains(t, html2, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokBeta+`">beta/</tg-button> 👈<br/>`)
	// Nested directory appears closed (blue) and not selected
	tokNested := app.TreeState.GetToken(nestedDir)
	require.Contains(t, html2, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokNested+`">nested1</tg-button><br/>`)
	// Bottom control row now contains Copy and get folder buttons for beta in tg-button-row preceded by separator hr
	require.NotContains(t, html2, "<table>")
	require.Contains(t, html2, "<hr/>")
	require.Contains(t, html2, `<tg-button-row align="left">`)
	require.Contains(t, html2, `<tg-button type="copy_text" style="success" text="`+subBeta+`">copy 'beta'</tg-button>`)
	require.Contains(t, html2, `<tg-button type="callback_data" style="success" data="t:zip:`+tokBeta+`">get 'beta.zip'</tg-button>`)

	// 3. Press parent folder 'beta' again to close it
	app.TreeState.ToggleExpanded(42, subBeta)
	html3, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)

	// Button collapses back to primary (blue) without trailing slash, still with 👈 as last clicked
	require.Contains(t, html3, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokBeta+`">beta</tg-button> 👈<br/>`)
	// Nested items are hidden again
	require.NotContains(t, html3, "nested1")
	// Copy and get folder buttons remain for last-clicked folder
	require.Contains(t, html3, "<hr/>")
	require.Contains(t, html3, `<tg-button type="copy_text" style="success" text="`+subBeta+`">copy 'beta'</tg-button>`)
	require.Contains(t, html3, `<tg-button type="callback_data" style="success" data="t:zip:`+tokBeta+`">get 'beta.zip'</tg-button>`)

	// 4. Select a file in the tree: it mixes copy path and get file buttons directly into the tree message, moving 👈 to the file
	app.TreeState.SetSelectedItem(42, file1Path)
	htmlFile, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	require.Contains(t, htmlFile, `<tg-button type="callback_data" data="t:file:`+tokFile1+`">file1.txt</tg-button> 👈<br/>`)
	require.NotContains(t, htmlFile, `>beta</tg-button> 👈`)
	require.Contains(t, htmlFile, "<hr/>")
	require.Contains(t, htmlFile, `<tg-button type="copy_text" style="success" text="`+file1Path+`">copy 'file1.txt'</tg-button>`)
	require.Contains(t, htmlFile, `<tg-button type="callback_data" style="success" data="t:send:`+tokFile1+`">get 'file1.txt'</tg-button>`)
}

func TestCreateZipArchive(t *testing.T) {
	tempDir := t.TempDir()
	sub := filepath.Join(tempDir, "myfolder")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "hello.txt"), []byte("hello world"), 0o644))

	data, err := createZipArchive(sub, 10*1024*1024)
	require.NoError(t, err)
	require.NotEmpty(t, data)
}

func TestBuildFileRichMessageHTML(t *testing.T) {
	filePath := "/path/to/test.txt"
	tok := "abc1"
	size := int64(1024)

	html := buildFileRichMessageHTML(filePath, tok, size)
	require.Contains(t, html, "test.txt")
	require.Contains(t, html, "1.0 KB")
	require.Contains(t, html, "<code>/path/to/test.txt</code>")
	require.NotContains(t, html, "<table>")
	require.Contains(t, html, "<hr/>")
	require.Contains(t, html, `<tg-button-row align="left">`)
	require.Contains(t, html, `<tg-button type="copy_text" style="success" text="/path/to/test.txt">copy 'test.txt'</tg-button>`)
	require.Contains(t, html, `<tg-button type="callback_data" style="success" data="t:send:abc1">get 'test.txt'</tg-button>`)
}

func TestBuildTreeRichMessageHTMLEmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	html, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	require.Contains(t, html, "(empty)")
}
