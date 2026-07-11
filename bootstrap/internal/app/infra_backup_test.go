package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/terraform"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// fakeTerraformOnPath installs a stub `terraform` executable that prints the
// contents of a sibling state.json for any invocation, and prepends it to PATH
// so Runner resolves it instead of the real binary.
func fakeTerraformOnPath(t *testing.T, stateContent string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateContent), 0644))

	if runtime.GOOS == "windows" {
		script := "@echo off\r\ntype \"%~dp0state.json\"\r\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "terraform.bat"), []byte(script), 0755))
	} else {
		script := "#!/bin/sh\ncat \"$(dirname \"$0\")/state.json\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "terraform"), []byte(script), 0755))
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newBackupTestApp(t *testing.T) *App {
	t.Helper()
	return &App{Cfg: &types.Config{}, Logger: zaptest.NewLogger(t)}
}

func listBackups(t *testing.T, tfDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(tfDir, "backups"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBackupState_LocalFileCopied(t *testing.T) {
	tfDir := t.TempDir()
	stateJSON := `{"version":4,"serial":7}`
	require.NoError(t, os.WriteFile(filepath.Join(tfDir, "terraform.tfstate"), []byte(stateJSON), 0644))

	app := newBackupTestApp(t)
	runner := terraform.NewRunner(tfDir, app.Logger)
	app.backupState(context.Background(), tfDir, runner, "tfstate")

	backups := listBackups(t, tfDir)
	require.Len(t, backups, 1)
	data, err := os.ReadFile(filepath.Join(tfDir, "backups", backups[0]))
	require.NoError(t, err)
	require.Equal(t, stateJSON, string(data))
}

func TestBackupState_RemoteBackendPulled(t *testing.T) {
	stateJSON := `{"version":4,"terraform_version":"1.14.3","serial":1,"resources":[]}`
	fakeTerraformOnPath(t, stateJSON)

	tfDir := t.TempDir() // no local terraform.tfstate → remote-backend path
	app := newBackupTestApp(t)
	runner := terraform.NewRunner(tfDir, app.Logger)
	app.backupState(context.Background(), tfDir, runner, "pre-destroy")

	backups := listBackups(t, tfDir)
	require.Len(t, backups, 1)
	require.Contains(t, backups[0], "pre-destroy-")
	data, err := os.ReadFile(filepath.Join(tfDir, "backups", backups[0]))
	require.NoError(t, err)
	require.JSONEq(t, stateJSON, string(data))
}

func TestBackupState_EmptyRemoteStateSkipped(t *testing.T) {
	fakeTerraformOnPath(t, "\n")

	tfDir := t.TempDir()
	app := newBackupTestApp(t)
	runner := terraform.NewRunner(tfDir, app.Logger)
	app.backupState(context.Background(), tfDir, runner, "tfstate")

	require.Empty(t, listBackups(t, tfDir))
}
