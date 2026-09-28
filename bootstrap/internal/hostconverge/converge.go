// Package hostconverge installs files on a host that talops manages outside
// the cluster, and never leaves it without the last configuration that worked:
// every write is backed up, validated before activation, verified after, and
// restored on failure.
package hostconverge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Now is the clock the backup suffix is read from; tests pin it.
var Now = time.Now

// RollbackTimeout bounds the time allowed for restore, re-activate, and re-verify
// operations during rollback. A cancelled caller (e.g., Ctrl-C during a long health
// gate) must not strand the host on an unverified configuration.
var RollbackTimeout = 15 * time.Minute

var pathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Runner executes a command on the remote host and returns its output or an error.
type Runner interface {
	Run(ctx context.Context, cmd string) (string, error)
}

// File describes a configuration file to install: its path, content, and optional mode.
// Mode of 0 means the mode is left as installed.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// Change describes a set of files to install and the validation, activation,
// and verification steps that follow.
type Change struct {
	Files    []File
	Validate func(ctx context.Context, r Runner) error
	Activate func(ctx context.Context, r Runner) error
	Verify   func(ctx context.Context, r Runner) error
}

// Result reports the outcome of an Apply operation: the backup suffix used,
// whether the configuration was rolled back after a failure, and any error
// that occurred during rollback itself.
type Result struct {
	BackupSuffix string
	RolledBack   bool
	RollbackErr  error
}

type installed struct {
	path      string
	hadBackup bool
}

// recoverCtx creates a context that ignores cancellation from the caller
// but is bounded by RollbackTimeout. This ensures that a cancelled caller
// does not strand the host on an unverified configuration.
func recoverCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), RollbackTimeout)
}

// abortInstall is the single unwind path for every in-loop failure (write,
// probe, backup, mv, chmod): it best-effort removes the temp file this
// iteration wrote — once the tee has run, tmp may exist and must not be left
// behind — and restores every file installed so far, both under a context
// that survives caller cancellation.
func abortInstall(ctx context.Context, r Runner, tmp string, done []installed, suffix string, cause error) error {
	rctx, cancel := recoverCtx(ctx)
	defer cancel()
	_, _ = r.Run(rctx, "sudo rm -f "+tmp)
	return errors.Join(cause, restore(rctx, r, done, suffix))
}

// Apply installs the files in c, backing them up first, then validates, activates,
// and verifies them in sequence. If any step fails, it restores the previous files
// (or deletes new ones that did not exist before) and re-activates and re-verifies
// the previous configuration. Content is limited to ~96 KiB (MAX_ARG_STRLEN for the
// single sh -c argument on the host). If Activate fails and Verify is nil, the
// failure is returned without restoration: the caller is responsible for recovery
// (this matches HAProxy's historical behavior).
func Apply(ctx context.Context, r Runner, c Change) (Result, error) {
	res := Result{BackupSuffix: Now().Format("20060102-150405")}

	for _, f := range c.Files {
		if !pathPattern.MatchString(f.Path) {
			return res, fmt.Errorf("reject unsafe path: %s", f.Path)
		}
	}

	var done []installed

	for _, f := range c.Files {
		dir := path.Dir(f.Path)
		base := path.Base(f.Path)
		tmp := path.Join(dir, "."+base+".new."+res.BackupSuffix)

		// Base64 encoding protects against shell metacharacters in the content.
		write := fmt.Sprintf("echo '%s' | base64 -d | sudo tee %s >/dev/null", base64.StdEncoding.EncodeToString(f.Content), tmp)
		if _, err := r.Run(ctx, write); err != nil {
			return res, abortInstall(ctx, r, tmp, done, res.BackupSuffix, fmt.Errorf("write temp config: %w", err))
		}

		// A missing file must not be backed up as if it existed: probe first
		// so an absent file skips the backup instead of copying something
		// that isn't there.
		probeOut, probeErr := r.Run(ctx, "sudo sh -c 'test -e "+f.Path+" || echo absent'")
		if probeErr != nil {
			return res, abortInstall(ctx, r, tmp, done, res.BackupSuffix, fmt.Errorf("back up %s: %w", f.Path, probeErr))
		}

		hadBackup := false
		if !strings.Contains(probeOut, "absent") {
			if _, bErr := r.Run(ctx, fmt.Sprintf("sudo cp -p %s %s.backup.%s", f.Path, f.Path, res.BackupSuffix)); bErr != nil {
				return res, abortInstall(ctx, r, tmp, done, res.BackupSuffix, fmt.Errorf("back up %s: %w", f.Path, bErr))
			}
			hadBackup = true
		}

		// Recorded before the move, not after: a failed mv may have
		// partially applied, so the file still needs restoring even though
		// mv never returned success.
		done = append(done, installed{path: f.Path, hadBackup: hadBackup})

		if _, err := r.Run(ctx, fmt.Sprintf("sudo mv %s %s", tmp, f.Path)); err != nil {
			return res, abortInstall(ctx, r, tmp, done, res.BackupSuffix, fmt.Errorf("install config: %w", err))
		}

		if f.Mode != 0 {
			if _, err := r.Run(ctx, fmt.Sprintf("sudo chmod %o %s", f.Mode.Perm(), f.Path)); err != nil {
				return res, abortInstall(ctx, r, tmp, done, res.BackupSuffix, fmt.Errorf("set mode on %s: %w", f.Path, err))
			}
		}
	}

	if c.Validate != nil {
		if err := c.Validate(ctx, r); err != nil {
			res.RolledBack = true
			rctx, cancel := recoverCtx(ctx)
			if rbErr := restore(rctx, r, done, res.BackupSuffix); rbErr != nil {
				res.RollbackErr = rbErr
				cancel()
				return res, fmt.Errorf("config validation failed and rollback also failed: validation=%w, rollback=%v", err, rbErr)
			}
			cancel()
			return res, fmt.Errorf("config validation failed (rolled back): %w", err)
		}
	}

	if c.Activate != nil {
		if err := c.Activate(ctx, r); err != nil {
			if c.Verify == nil {
				return res, err
			}
			return rollBackAfter(ctx, r, c, done, res, err)
		}
	}

	if c.Verify != nil {
		if err := c.Verify(ctx, r); err != nil {
			return rollBackAfter(ctx, r, c, done, res, err)
		}
	}
	return res, nil
}

// rollBackAfter restores the previous files and proves the previous config is
// serving again. A second failure is reported in its own field so the caller
// can tell "rolled back, healthy" from "rolled back, still down".
func rollBackAfter(ctx context.Context, r Runner, c Change, done []installed, res Result, cause error) (Result, error) {
	res.RolledBack = true

	rctx, cancel := recoverCtx(ctx)
	defer cancel()

	if err := restore(rctx, r, done, res.BackupSuffix); err != nil {
		res.RollbackErr = err
		return res, fmt.Errorf("%w; rollback failed to restore files: %v", cause, err)
	}
	if c.Activate != nil {
		if err := c.Activate(rctx, r); err != nil {
			res.RollbackErr = err
			return res, fmt.Errorf("%w; rollback restored files but could not activate them: %v", cause, err)
		}
	}
	if c.Verify != nil {
		if err := c.Verify(rctx, r); err != nil {
			res.RollbackErr = err
			return res, fmt.Errorf("%w; rollback restored the previous config, which is unhealthy too: %v", cause, err)
		}
	}
	return res, fmt.Errorf("%w (rolled back)", cause)
}

func restore(ctx context.Context, r Runner, done []installed, suffix string) error {
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		f := done[i]
		cmd := fmt.Sprintf("sudo rm -f %s", f.path)
		if f.hadBackup {
			// No -p here: HAProxy's rollback-also-fails test keys on this
			// exact "sudo cp <path>.backup.<suffix> <path>" prefix.
			cmd = fmt.Sprintf("sudo cp %s.backup.%s %s", f.path, suffix, f.path)
		}
		if _, err := r.Run(ctx, cmd); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Unchanged checks whether all files have the same content as their on-disk versions.
// Runner errors are returned; missing files are treated as changed (false, nil).
// Output that is neither "missing" nor a hash is an error: a runner returns
// combined output, so it is a failed read or stderr noise, and calling that
// "changed" would restart a server over a read that never happened.
func Unchanged(ctx context.Context, r Runner, files []File) (bool, error) {
	for _, f := range files {
		if !pathPattern.MatchString(f.Path) {
			return false, fmt.Errorf("reject unsafe path: %s", f.Path)
		}

		out, err := r.Run(ctx, "sudo sh -c 'if test -e "+f.Path+"; then sha256sum "+f.Path+" 2>/dev/null; else echo missing; fi'")
		if err != nil {
			return false, err
		}

		out = strings.TrimSpace(out)
		if out == "missing" {
			return false, nil
		}

		got, _, _ := strings.Cut(out, " ")
		if !sha256Hex.MatchString(got) {
			return false, fmt.Errorf("hash %s: unexpected output %q", f.Path, out)
		}
		want := sha256.Sum256(f.Content)
		if got != hex.EncodeToString(want[:]) {
			return false, nil
		}
	}
	return true, nil
}
