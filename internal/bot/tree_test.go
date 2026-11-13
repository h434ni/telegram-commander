package bot

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
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
	require.NoError(t, os.WriteFile(filepath.Join(subBeta, "beta_file.txt"), []byte("data"), 0o644))

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
	// Nested directory appears closed (blue) and not selected, formatted with compact │├
	tokNested := app.TreeState.GetToken(nestedDir)
	require.Contains(t, html2, `│├<tg-button type="callback_data" style="primary" data="t:tog:`+tokNested+`">nested1</tg-button><br/>`)
	// Bottom control row now contains Copy and get folder buttons for beta in tg-button-row preceded by separator hr and ~/ path
	require.NotContains(t, html2, "<table>")
	require.Contains(t, html2, "<hr/>")
	require.Contains(t, html2, "<p><code>~/beta</code></p>")
	require.Contains(t, html2, `<tg-button-row align="left">`)
	require.Contains(t, html2, `<tg-button type="copy_text" style="success" text="`+subBeta+`">copy full path</tg-button>`)
	require.Contains(t, html2, `<tg-button type="callback_data" style="success" data="t:zip:`+tokBeta+`">download folder</tg-button>`)

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
	require.Contains(t, html3, "<p><code>~/beta</code></p>")
	require.Contains(t, html3, `<tg-button type="copy_text" style="success" text="`+subBeta+`">copy full path</tg-button>`)
	require.Contains(t, html3, `<tg-button type="callback_data" style="success" data="t:zip:`+tokBeta+`">download folder</tg-button>`)

	// 4. Select a file in the tree: it mixes copy path and get file buttons directly into the tree message, moving 👈 to the file
	app.TreeState.SetSelectedItem(42, file1Path)
	htmlFile, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	require.Contains(t, htmlFile, `<tg-button type="callback_data" data="t:file:`+tokFile1+`">file1.txt</tg-button> 👈<br/>`)
	require.NotContains(t, htmlFile, `>beta</tg-button> 👈`)
	require.Contains(t, htmlFile, "<hr/>")
	require.Contains(t, htmlFile, "<p><code>~/file1.txt</code></p>")
	require.Contains(t, htmlFile, `<tg-button type="copy_text" style="success" text="`+file1Path+`">copy full path</tg-button>`)
	require.Contains(t, htmlFile, `<tg-button type="callback_data" style="success" data="t:send:`+tokFile1+`">download file</tg-button>`)
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

func TestHasGit(t *testing.T) {
	tempDir := t.TempDir()
	require.False(t, hasGit(tempDir))

	gitDir := filepath.Join(tempDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.True(t, hasGit(tempDir))

	tempDir2 := t.TempDir()
	gitFile := filepath.Join(tempDir2, ".git")
	require.NoError(t, os.WriteFile(gitFile, []byte("gitdir: /path/to/worktree\n"), 0o644))
	require.True(t, hasGit(tempDir2))
}

func TestCreateGitZipArchive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "myrepo")
	require.NoError(t, os.Mkdir(repoDir, 0o755))

	// git init
	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	require.NoError(t, cmd.Run())

	// Configure dummy git user
	cmdUser := exec.Command("git", "config", "user.name", "Tester")
	cmdUser.Dir = repoDir
	require.NoError(t, cmdUser.Run())
	cmdEmail := exec.Command("git", "config", "user.email", "tester@example.com")
	cmdEmail.Dir = repoDir
	require.NoError(t, cmdEmail.Run())

	// Test repo without commits returns friendly error
	_, err := createGitZipArchive(context.Background(), repoDir, 10*1024*1024)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no commits found")

	// Add tracked file and gitignore
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("tracked-content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "ignored.txt"), []byte("ignored-content"), 0o644))

	cmdAdd := exec.Command("git", "add", "tracked.txt", ".gitignore")
	cmdAdd.Dir = repoDir
	require.NoError(t, cmdAdd.Run())

	cmdCommit := exec.Command("git", "commit", "-m", "init commit")
	cmdCommit.Dir = repoDir
	require.NoError(t, cmdCommit.Run())

	// Archive git repo
	zipData, err := createGitZipArchive(context.Background(), repoDir, 10*1024*1024)
	require.NoError(t, err)
	require.NotEmpty(t, zipData)

	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}

	// Should contain tracked.txt and .gitignore with prefix myrepo/
	require.True(t, names["myrepo/tracked.txt"], "tracked.txt should be in zip")
	require.True(t, names["myrepo/.gitignore"], ".gitignore should be in zip")
	// Should NOT contain ignored.txt
	require.False(t, names["myrepo/ignored.txt"], "ignored.txt must be ignored")
	// Should NOT contain .git
	for name := range names {
		require.False(t, strings.Contains(name, ".git/"), ".git internal directory must not be in archive")
	}
}

func TestTreeAdvDlButton_Presence(t *testing.T) {
	tempDir := t.TempDir()
	app := &App{
		TreeState: NewTreeStateManager(tempDir),
	}

	folderNoGit := filepath.Join(tempDir, "plain")
	require.NoError(t, os.Mkdir(folderNoGit, 0o755))

	folderWithGit := filepath.Join(tempDir, "gitrepo")
	require.NoError(t, os.Mkdir(folderWithGit, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(folderWithGit, ".git"), 0o755))

	tokNoGit := app.TreeState.GetToken(folderNoGit)
	tokWithGit := app.TreeState.GetToken(folderWithGit)

	// Select folder without git: download folder is shown, adv dl is NOT shown
	app.TreeState.SetSelectedItem(1, folderNoGit)
	htmlNoGit, err := app.buildTreeRichMessageHTML(tempDir, 1)
	require.NoError(t, err)
	require.Contains(t, htmlNoGit, `data="t:zip:`+tokNoGit+`">download folder</tg-button>`)
	require.NotContains(t, htmlNoGit, `adv dl`)

	// Select folder with git: download folder AND adv dl are shown
	app.TreeState.SetSelectedItem(1, folderWithGit)
	htmlWithGit, err := app.buildTreeRichMessageHTML(tempDir, 1)
	require.NoError(t, err)
	require.Contains(t, htmlWithGit, `data="t:zip:`+tokWithGit+`">download folder</tg-button>`)
	require.Contains(t, htmlWithGit, `data="t:gzip:`+tokWithGit+`">adv dl</tg-button>`)
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
	require.Contains(t, html, `<tg-button type="copy_text" style="success" text="/path/to/test.txt">copy full path</tg-button>`)
	require.Contains(t, html, `<tg-button type="callback_data" style="success" data="t:send:abc1">download file</tg-button>`)
	require.Contains(t, html, "<p>file operations:</p>")
	require.Contains(t, html, `<tg-button type="callback_data" style="success" data="t:ren:abc1">rename</tg-button>`)
	require.Contains(t, html, `<tg-button type="callback_data" style="success" data="t:edit:abc1">edit</tg-button>`)
}

func TestTreeStateManager_Awaiting(t *testing.T) {
	mgr := NewTreeStateManager("/home/ec2-user")
	userID := int64(999)

	_, ok := mgr.GetAwaiting(userID)
	require.False(t, ok)

	mgr.SetAwaiting(userID, TreeActionEdit, "/home/ec2-user/test.txt", 101)
	aw, ok := mgr.GetAwaiting(userID)
	require.True(t, ok)
	require.Equal(t, TreeActionEdit, aw.Action)
	require.Equal(t, "/home/ec2-user/test.txt", aw.TargetPath)
	require.Equal(t, 101, aw.PromptMsgID)

	mgr.ClearAwaiting(userID)
	_, ok = mgr.GetAwaiting(userID)
	require.False(t, ok)

	mgr.SetAwaiting(userID, TreeActionRename, "/home/ec2-user/test.txt", 102)
	aw, ok = mgr.GetAwaiting(userID)
	require.True(t, ok)
	require.Equal(t, TreeActionRename, aw.Action)

	mgr.ClearUserMessage(userID)
	_, ok = mgr.GetAwaiting(userID)
	require.False(t, ok)
}

func TestFormatTildePath(t *testing.T) {
	require.Equal(t, "~/", formatTildePath("/home/user", "/home/user", ""))
	require.Equal(t, "~/apps/test", formatTildePath("/home/user/apps/test", "/home/user", ""))
	require.Equal(t, "~/inner/file.txt", formatTildePath("/tmp/root/inner/file.txt", "/home/user", "/tmp/root"))
	require.Equal(t, "/var/log/syslog", formatTildePath("/var/log/syslog", "/home/user", "/tmp/root"))
}

func TestBuildTreeRichMessageHTMLEmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	html, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	require.Contains(t, html, "(empty)")
}

func TestBuildTreeRichMessageHTML_LastFolderIndentation(t *testing.T) {
	tempDir := t.TempDir()
	lastFolder := filepath.Join(tempDir, "lastfolder")
	require.NoError(t, os.Mkdir(lastFolder, 0o755))
	subLast := filepath.Join(lastFolder, "sublast")
	require.NoError(t, os.Mkdir(subLast, 0o755))
	childFile := filepath.Join(subLast, "child.txt")
	require.NoError(t, os.WriteFile(childFile, []byte("content"), 0o644))

	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	app.TreeState.ToggleExpanded(42, lastFolder)
	app.TreeState.ToggleExpanded(42, subLast)
	app.TreeState.SetSelectedItem(42, lastFolder)

	html, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	// Parent folder is the root last folder (branch └)
	tokLast := app.TreeState.GetToken(lastFolder)
	require.Contains(t, html, `└<tg-button type="callback_data" style="primary" data="t:tog:`+tokLast+`">lastfolder/</tg-button> 👈<br/>`)
	// Children of root last folder shifted 1 space forward: "\u00a0\u00a0└"
	tokSub := app.TreeState.GetToken(subLast)
	require.Contains(t, html, "\u00a0\u00a0└<tg-button type=\"callback_data\" style=\"primary\" data=\"t:tog:"+tokSub+"\">sublast/</tg-button><br/>")
	// In that content, the nested last folder also shifts forward by 2 spaces: "\u00a0\u00a0\u00a0\u00a0└"
	tokChild := app.TreeState.GetToken(childFile)
	require.Contains(t, html, "\u00a0\u00a0\u00a0\u00a0└<tg-button type=\"callback_data\" data=\"t:file:"+tokChild+"\">child.txt</tg-button><br/>")
	// Separator preceded by a new line (<br/>)
	require.Contains(t, html, "<br/>\n<hr/>\n<p><code>")

	// Also verify a last folder inside a non-last folder (prefix has "│") gets 2 spaces, not 1
	alphaFolder := filepath.Join(tempDir, "alpha")
	require.NoError(t, os.Mkdir(alphaFolder, 0o755))
	alphaLast := filepath.Join(alphaFolder, "alphalast")
	require.NoError(t, os.Mkdir(alphaLast, 0o755))
	alphaChild := filepath.Join(alphaLast, "leaf.txt")
	require.NoError(t, os.WriteFile(alphaChild, []byte("leaf"), 0o644))

	app.TreeState.ToggleExpanded(42, alphaFolder)
	app.TreeState.ToggleExpanded(42, alphaLast)
	htmlAlpha, err := app.buildTreeRichMessageHTML(tempDir, 42)
	require.NoError(t, err)
	tokLeaf := app.TreeState.GetToken(alphaChild)
	// Children of alphaLast get "│\u00a0\u00a0└" (2 spaces after │), never just 1 space
	require.Contains(t, htmlAlpha, "│\u00a0\u00a0└<tg-button type=\"callback_data\" data=\"t:file:"+tokLeaf+"\">leaf.txt</tg-button><br/>")
}

func TestBuildTreeRichMessageHTML_RootVisibleAndBackButton(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "targetdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	innerFile := filepath.Join(subDir, "inner.txt")
	require.NoError(t, os.WriteFile(innerFile, []byte("data"), 0o644))

	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	app.TreeState.SetDefaultPath(42, tempDir)
	app.TreeState.SetRootFolderVisible(42, true)
	app.TreeState.SetExpanded(42, subDir, true)
	app.TreeState.SetSelectedFolder(42, subDir)
	app.TreeState.SetUserPath(42, subDir)

	html, err := app.buildTreeRichMessageHTML(subDir, 42)
	require.NoError(t, err)

	tokSub := app.TreeState.GetToken(subDir)
	// 1. Root folder is rendered on top with 👈
	require.Contains(t, html, `<tg-button type="callback_data" style="primary" data="t:tog:`+tokSub+`">targetdir/</tg-button> 👈<br/>`)
	// 2. Child items are listed under it
	require.Contains(t, html, "inner.txt")
	// 3. Action buttons for targetdir (including per-folder show hidden)
	require.Contains(t, html, "copy full path")
	require.Contains(t, html, "download folder")
	require.Contains(t, html, `data="t:hid:`+tokSub+`">show hidden</tg-button>`)
	// 4. Transparent back button and settings button right after the tree without additional separator line
	require.Contains(t, html, `data="t:home">back</tg-button>`)
	require.Contains(t, html, `<tg-button type="callback_data" data="t:cfg">⚙</tg-button>`)
	require.True(t, strings.Index(html, `data="t:home">back</tg-button>`) < strings.Index(html, "copy full path"), "back button should appear before action buttons")
}

func TestTreeShowHiddenToggle_PerFolderAndSettings(t *testing.T) {
	tempDir := t.TempDir()
	app := &App{
		TreeState: NewTreeStateManager(tempDir),
	}

	subA := filepath.Join(tempDir, "folderA")
	subB := filepath.Join(tempDir, "folderB")
	require.NoError(t, os.MkdirAll(subA, 0o755))
	require.NoError(t, os.MkdirAll(subB, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(subA, "visibleA.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subA, ".hiddenA.txt"), []byte("ha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subB, "visibleB.txt"), []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subB, ".hiddenB.txt"), []byte("hb"), 0o644))

	userID := int64(99)
	tokA := app.TreeState.GetToken(subA)
	tokB := app.TreeState.GetToken(subB)

	app.TreeState.SetExpanded(userID, subA, true)
	app.TreeState.SetExpanded(userID, subB, true)
	app.TreeState.SetSelectedFolder(userID, subA)

	// 1. By default, hidden files in both folders are concealed, and show hidden is in folderA's action section
	html1, err := app.buildTreeRichMessageHTML(tempDir, userID)
	require.NoError(t, err)
	require.Contains(t, html1, "visibleA.txt")
	require.NotContains(t, html1, ".hiddenA.txt")
	require.Contains(t, html1, "visibleB.txt")
	require.NotContains(t, html1, ".hiddenB.txt")
	require.Contains(t, html1, `<tg-button type="callback_data" style="success" data="t:hid:`+tokA+`">show hidden</tg-button>`)
	// Settings button is transparent when settings keyboard is not visible
	require.Contains(t, html1, `<tg-button type="callback_data" data="t:cfg">⚙</tg-button>`)

	// 2. Toggle hidden for folderA ONLY -> folderA shows .hiddenA.txt, folderB still hides .hiddenB.txt
	nowShow := app.TreeState.ToggleFolderShowHidden(userID, subA)
	require.True(t, nowShow)

	html2, err := app.buildTreeRichMessageHTML(tempDir, userID)
	require.NoError(t, err)
	require.Contains(t, html2, ".hiddenA.txt")
	require.NotContains(t, html2, ".hiddenB.txt")
	require.Contains(t, html2, `<tg-button type="callback_data" style="success" data="t:hid:`+tokA+`">hide hidden</tg-button>`)

	// Select folderB -> folderB shows "show hidden"
	app.TreeState.SetSelectedFolder(userID, subB)
	htmlB, err := app.buildTreeRichMessageHTML(tempDir, userID)
	require.NoError(t, err)
	require.Contains(t, htmlB, `<tg-button type="callback_data" style="success" data="t:hid:`+tokB+`">show hidden</tg-button>`)

	// 3. Toggle settings visibility -> ⚙ button turns blue (style="primary")
	app.TreeState.ToggleSettingsVisible(userID)
	htmlCfg, err := app.buildTreeRichMessageHTML(tempDir, userID)
	require.NoError(t, err)
	require.Contains(t, htmlCfg, `<tg-button type="callback_data" style="primary" data="t:cfg">⚙</tg-button>`)

	// 4. Switch global button style to emoji -> action buttons render emojis only
	app.TreeState.ToggleButtonStyleEmoji()
	htmlEmoji, err := app.buildTreeRichMessageHTML(tempDir, userID)
	require.NoError(t, err)
	require.Contains(t, htmlEmoji, `>📋</tg-button>`)
	require.Contains(t, htmlEmoji, `data="t:zip:`+tokB+`">📥</tg-button>`)
	require.Contains(t, htmlEmoji, `data="t:hid:`+tokB+`">👁</tg-button>`)
}
