package secrets

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// RepoRoot walks up from the current directory to find the repository root,
// identified by a .git entry (preferred) or a .sops.yaml. Returns "" if none is
// found. Anchoring vault operations to this root makes them independent of the
// directory talops is invoked from, preventing a split vault.
func RepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	var sopsFallback string
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if sopsFallback == "" {
			if _, err := os.Stat(filepath.Join(dir, ".sops.yaml")); err == nil {
				sopsFallback = dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return sopsFallback
}

// GitState summarizes the vault's git situation for safety checks.
type GitState struct {
	IsRepo      bool     // current tree is a git work tree
	HasUpstream bool     // the current branch tracks an upstream
	Behind      int      // commits the local branch is behind its upstream
	DirtyVault  []string // tracked vault paths with uncommitted changes
}

// VaultGitState inspects git for the given vault paths. It performs no network
// access — "Behind" reflects the last-fetched upstream ref, so a `git pull`
// (or fetch) beforehand makes it accurate. Errors from individual git commands
// are treated as "no information" rather than failures.
func VaultGitState(ctx context.Context, vaultPaths []string) GitState {
	var st GitState
	if out, err := git(ctx, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return st
	}
	st.IsRepo = true

	if out, err := git(ctx, "rev-list", "--count", "HEAD..@{u}"); err == nil {
		st.HasUpstream = true
		if n, convErr := strconv.Atoi(strings.TrimSpace(out)); convErr == nil {
			st.Behind = n
		}
	}

	if len(vaultPaths) > 0 {
		args := append([]string{"status", "--porcelain", "--"}, vaultPaths...)
		if out, err := git(ctx, args...); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					st.DirtyVault = append(st.DirtyVault, line)
				}
			}
		}
	}
	return st
}

// FetchUpstream runs `git fetch` so that VaultGitState's behind-count reflects
// the true remote state. Best-effort: returns an error only so callers can
// surface it; a failure (offline, no remote) is not fatal to the caller.
func FetchUpstream(ctx context.Context) error {
	_, err := git(ctx, "fetch", "--quiet")
	return err
}

func git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}
