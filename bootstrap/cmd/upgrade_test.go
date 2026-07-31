package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/upgrade"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The refusal paths must be decided before talosctl is reachable, so these run
// without any cluster and assert nothing was executed.
func TestUpgradeK8sRefusalPaths(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  []string
	}{
		{
			name:     "prune opt-in without confirmation",
			args:     []string{"--to", "1.36.3", "--apply", "--allow-manifest-prune"},
			wantExit: 2,
			wantOut:  []string{"error:", "--confirm-manifest-prune", "Nothing was run", "help:"},
		},
		{
			name:     "confirmation without opt-in",
			args:     []string{"--to", "1.36.3", "--apply", "--confirm-manifest-prune"},
			wantExit: 2,
			wantOut:  []string{"error:", "--allow-manifest-prune", "Nothing was run"},
		},
		{
			name:     "missing required version",
			args:     []string{"--apply"},
			wantExit: 2,
			wantOut:  []string{"error:", "--to is required", "talops upgrade-k8s --to <version>"},
		},
		{
			name:     "unreadable desired-from is not an empty desired set",
			args:     []string{"--to", "1.36.3", "--node", "192.168.1.50", "--desired-from", "does-not-exist.txt"},
			wantExit: 2,
			wantOut:  []string{"error:", "--desired-from", "could not be read"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := setupTestApp(t)
			cmd := upgradeK8sCmd(a)

			out, err := execute(t, cmd, tt.args...)

			require.Error(t, err)
			assert.Equal(t, tt.wantExit, ExitCode(err))
			for _, want := range tt.wantOut {
				assert.Contains(t, out, want)
			}
			// A refusal must never have produced a talosctl command line.
			assert.NotContains(t, out, "command: talosctl")
		})
	}
}

// The CLI already has a global -d/--dry-run. Combining it with --apply is a
// contradictory request, and resolving it either way silently would leave the
// operator with a wrong model of whether the cluster was mutated.
func TestUpgradeK8sRefusesContradictoryDryRunAndApply(t *testing.T) {
	a := setupTestApp(t)
	a.Cfg.DryRun = true
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--apply")

	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, out, "--dry-run")
	assert.Contains(t, out, "--apply")
	assert.NotContains(t, out, "command: talosctl")
}

// stubRunner replaces the real talosctl for the duration of a test. No test in
// this package may execute talosctl: it targets a live cluster, and upgrade-k8s
// is the single most destructive command talops can issue.
func stubRunner(t *testing.T, out string, err error) *[]string {
	t.Helper()

	var got []string
	original := talosctlRunnerFor
	talosctlRunnerFor = func(_ *app.App) upgrade.Runner {
		return func(args []string) ([]byte, error) {
			got = args
			return []byte(out), err
		}
	}
	// The inventory read shells out to kubectl, which is equally live.
	originalInventory := inventoryReaderFor
	inventoryReaderFor = func(_ context.Context, _ *app.App) upgrade.InventoryReader { return nil }

	t.Cleanup(func() {
		talosctlRunnerFor = original
		inventoryReaderFor = originalInventory
	})
	return &got
}

// The global --dry-run agreeing with the default is not an error.
func TestUpgradeK8sAcceptsGlobalDryRunWithoutApply(t *testing.T) {
	a := setupTestApp(t)
	a.Cfg.DryRun = true
	got := stubRunner(t, "updating manifests (dry run)", nil)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--node", "192.168.1.50")

	require.NoError(t, err)
	assert.Contains(t, *got, "--dry-run")
	assert.Contains(t, *got, "--manifests-no-prune")
	assert.Contains(t, out, "mode: dry-run")
}

// The preview is the default, so an invocation with no mode flag at all must not
// mutate — this is the single most important behaviour in the package.
func TestUpgradeK8sDefaultsToDryRun(t *testing.T) {
	a := setupTestApp(t)
	got := stubRunner(t, "updating manifests (dry run)", nil)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--node", "192.168.1.50")

	require.NoError(t, err)
	assert.Equal(t, []string{
		"-n", "192.168.1.50", "upgrade-k8s", "--to", "1.36.3", "--manifests-no-prune", "--dry-run",
	}, *got)
	assert.Contains(t, out, "manifests_no_prune: true")
}

// talosctl rejects upgrade-k8s across multiple nodes with a raw error. Requiring
// --node keeps that from leaking and guarantees exactly one target.
func TestUpgradeK8sRequiresNode(t *testing.T) {
	a := setupTestApp(t)
	got := stubRunner(t, "", nil)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3")

	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, out, "--node is required")
	assert.Nil(t, *got, "talosctl must not be invoked without a resolved node")
}

// An --apply run must carry the guard and drop --dry-run, and must still be the
// argv the preview showed.
func TestUpgradeK8sApplyKeepsTheGuard(t *testing.T) {
	a := setupTestApp(t)
	got := stubRunner(t, "", nil)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--node", "192.168.1.50", "--apply")

	require.NoError(t, err)
	assert.Contains(t, *got, "--manifests-no-prune")
	assert.NotContains(t, *got, "--dry-run")
	assert.Contains(t, out, "mode: apply")
}

// Both prune flags together are the only way the guard comes off.
func TestUpgradeK8sConfirmedPruneDropsTheGuard(t *testing.T) {
	a := setupTestApp(t)
	got := stubRunner(t, "", nil)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--node", "192.168.1.50", "--apply",
		"--allow-manifest-prune", "--confirm-manifest-prune")

	require.NoError(t, err)
	assert.NotContains(t, *got, "--manifests-no-prune")
	assert.Contains(t, out, "manifests_no_prune: false")
	assert.Contains(t, out, "warning:")
}

// A talosctl failure must exit 1, not 2 — it is a failed operation, not a
// malformed request — and the report must still be printed.
func TestUpgradeK8sRunFailureExitsOne(t *testing.T) {
	a := setupTestApp(t)
	stubRunner(t, "manifest stage failed", errors.New("exit status 1"))
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--to", "1.36.3", "--node", "192.168.1.50", "--apply")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "command: talosctl")
	assert.Contains(t, out, "error:")
}

// An unknown flag must be rejected by name rather than ignored: silently
// dropping it would return plausible output for a different request.
func TestUpgradeK8sRejectsUnknownFlag(t *testing.T) {
	a := setupTestApp(t)
	cmd := upgradeK8sCmd(a)

	_, err := execute(t, cmd, "--to", "1.36.3", "--manifests-no-prune")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag")
}

func TestUpgradeK8sRejectsPositionalArgs(t *testing.T) {
	a := setupTestApp(t)
	cmd := upgradeK8sCmd(a)

	_, err := execute(t, cmd, "--to", "1.36.3", "stray-argument")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestReadDesiredKeys(t *testing.T) {
	dir := t.TempDir()

	t.Run("skips blanks and comments", func(t *testing.T) {
		path := filepath.Join(dir, "keys.txt")
		require.NoError(t, os.WriteFile(path,
			[]byte("# a comment\n\n_kubelet-serving-cert-approver__Namespace\n  kube-system_coredns__Deployment  \n"), 0o600))

		keys, err := readDesiredKeys(path)

		require.NoError(t, err)
		assert.Equal(t, []string{
			"_kubelet-serving-cert-approver__Namespace",
			"kube-system_coredns__Deployment",
		}, keys)
	})

	t.Run("no path means no desired set", func(t *testing.T) {
		keys, err := readDesiredKeys("")
		require.NoError(t, err)
		assert.Nil(t, keys)
	})

	// A file of only comments is an operator mistake, not an assertion that
	// every inventory key is prunable.
	t.Run("a file with no keys is a usage error", func(t *testing.T) {
		path := filepath.Join(dir, "empty.txt")
		require.NoError(t, os.WriteFile(path, []byte("# nothing here\n"), 0o600))

		_, err := readDesiredKeys(path)

		require.Error(t, err)
		assert.ErrorIs(t, err, upgrade.ErrUsage)
		assert.Equal(t, 2, ExitCode(err))
	})
}

func TestExitCode(t *testing.T) {
	assert.Equal(t, 0, ExitCode(nil))
	assert.Equal(t, 2, ExitCode(exitUsage{err: upgrade.ErrUsage}))
	assert.Equal(t, 2, ExitCode(upgrade.ErrUsage))
	assert.Equal(t, 1, ExitCode(assert.AnError))
}

// The help output is the agent's one-turn recovery path, so the safety-relevant
// flags and the default have to be discoverable there.
func TestUpgradeK8sHelpDocumentsTheGuard(t *testing.T) {
	a := setupTestApp(t)
	cmd := upgradeK8sCmd(a)

	out, err := execute(t, cmd, "--help")

	require.NoError(t, err)
	for _, want := range []string{
		"--manifests-no-prune",
		"--allow-manifest-prune",
		"--confirm-manifest-prune",
		"--apply",
		"--desired-from",
		"default is a preview that mutates nothing",
	} {
		assert.Contains(t, out, want)
	}
}
