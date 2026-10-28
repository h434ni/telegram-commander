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

func TestBuildTreeRichText(t *testing.T) {
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
	text, err := app.buildTreeRichText(tempDir)
	require.NoError(t, err)

	require.Contains(t, text, "Folder Explorer")
	require.Contains(t, text, tempDir)
	require.Contains(t, text, "📁 <b>alpha</b>")
	require.Contains(t, text, "📁 <b>beta</b>")
	require.Contains(t, text, "📁 nested1")
	require.Contains(t, text, "📄 file1.txt")
	require.Contains(t, text, "3 directories, 2 files")

	// Rich text commands in front of each directory
	tokAlpha := app.TreeState.GetToken(sub2)
	tokBeta := app.TreeState.GetToken(sub1)
	require.Contains(t, text, "/cd_"+tokAlpha)
	require.Contains(t, text, "/cd_"+tokBeta)

	// Navigation commands
	require.Contains(t, text, "<b>Up:</b> /cd_up")
	require.Contains(t, text, "<b>Home:</b> /cd_home")
	require.Contains(t, text, "<b>Refresh:</b> /cd_ref")
	require.Contains(t, text, "<b>Close:</b> /cd_close")
}

func TestBuildTreeRichTextEmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	app := NewApp(&config.Config{}, function.NewRegistry(), &executor.FakeExecutor{}, nil)
	text, err := app.buildTreeRichText(tempDir)
	require.NoError(t, err)
	require.Contains(t, text, "(empty directory)")
	require.Contains(t, text, "0 directories, 0 files")
}
