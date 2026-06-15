package secrets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "git %v", args)
	}
}

func TestRepoRootFindsGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitInit(t, root)
	sub := filepath.Join(root, "bootstrap", "cmd")
	require.NoError(t, os.MkdirAll(sub, 0755))
	t.Chdir(sub)

	got := RepoRoot()
	// macOS tmp dirs are symlinked (/var -> /private/var); compare resolved.
	gotResolved, _ := filepath.EvalSymlinks(got)
	rootResolved, _ := filepath.EvalSymlinks(root)
	require.Equal(t, rootResolved, gotResolved)
}

func TestVaultGitStateNonRepo(t *testing.T) {
	t.Chdir(t.TempDir())
	st := VaultGitState(context.Background(), nil)
	require.False(t, st.IsRepo)
}

func TestVaultGitStateDetectsDirtyVault(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitInit(t, root)
	t.Chdir(root)

	vaultFile := "secrets.enc.yaml"
	require.NoError(t, os.WriteFile(vaultFile, []byte("data: ENC[...]\n"), 0644))

	st := VaultGitState(context.Background(), []string{vaultFile})
	require.True(t, st.IsRepo)
	require.NotEmpty(t, st.DirtyVault, "newly added untracked vault file should be reported dirty")
}
