package hostconverge

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner records commands and fails any whose text contains a key of fail.
// The out map can return "absent" or "missing" for probe/check outputs.
// It returns ctx.Err() when the context is done, to simulate real behavior.
type fakeRunner struct {
	cmds []string
	fail map[string]error
	out  map[string]string
	// cancelOn, if set, fires cancelFn the first time a command containing
	// this substring is about to run, simulating a caller cancelling while a
	// command is in flight.
	cancelOn string
	cancelFn context.CancelFunc
}

func (f *fakeRunner) Run(ctx context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	if f.cancelOn != "" && strings.Contains(cmd, f.cancelOn) {
		f.cancelOn = ""
		f.cancelFn()
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	for k, err := range f.fail {
		if strings.Contains(cmd, k) {
			return "", err
		}
	}
	for k, o := range f.out {
		if strings.Contains(cmd, k) {
			return o, nil
		}
	}
	return "", nil
}

func fixedNow(t *testing.T) {
	t.Helper()
	old := Now
	Now = func() time.Time { return time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC) }
	t.Cleanup(func() { Now = old })
}

func TestApplyWritesBacksUpInstallsInHAProxysOrder(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{}
	var activated bool
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/haproxy/haproxy.cfg", Content: []byte("cfg")}},
		Activate: func(context.Context, Runner) error { activated = true; return nil },
	})
	require.NoError(t, err)
	assert.True(t, activated)
	require.GreaterOrEqual(t, len(r.cmds), 4)
	assert.Contains(t, r.cmds[0], "base64 -d | sudo tee /etc/haproxy/.haproxy.cfg.new.20260928-010203 >/dev/null")
	assert.Equal(t, "sudo sh -c 'test -e /etc/haproxy/haproxy.cfg || echo absent'", r.cmds[1])
	assert.Equal(t, "sudo cp -p /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.backup.20260928-010203", r.cmds[2])
	assert.Equal(t, "sudo mv /etc/haproxy/.haproxy.cfg.new.20260928-010203 /etc/haproxy/haproxy.cfg", r.cmds[3])
}

func TestApplyWritesContentVerbatim(t *testing.T) {
	r := &fakeRunner{}
	content := "a 'quoted' $HOME\nline2 `x` \"y\""
	_, err := Apply(t.Context(), r, Change{Files: []File{{Path: "/x/y", Content: []byte(content)}}})
	require.NoError(t, err)
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	assert.Contains(t, r.cmds[0], "echo '"+enc+"' | base64 -d | sudo tee /x/.y.new")
}

func TestApplyValidateFailureRestoresAndDoesNotActivate(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{}
	activated := false
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Validate: func(context.Context, Runner) error { return errors.New("bad") },
		Activate: func(context.Context, Runner) error { activated = true; return nil },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed (rolled back)")
	assert.False(t, activated)
	assert.Contains(t, r.cmds, "sudo cp /etc/a.backup.20260928-010203 /etc/a")
}

func TestApplyValidateAndRollbackFailBoth(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{fail: map[string]error{"sudo cp /etc/a.backup.": errors.New("cannot stat")}}
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Validate: func(context.Context, Runner) error { return errors.New("bad") },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed and rollback also failed")
}

func TestApplyWriteFailureMentionsTempConfig(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"base64 -d": errors.New("permission denied")}}
	_, err := Apply(t.Context(), r, Change{Files: []File{{Path: "/etc/a", Content: []byte("x")}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write temp config")
}

func TestApplyVerifyFailureRestoresReactivatesReverifies(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{}
	activations, verifies := 0, 0
	res, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Activate: func(context.Context, Runner) error { activations++; return nil },
		Verify: func(context.Context, Runner) error {
			verifies++
			if verifies == 1 {
				return errors.New("unhealthy")
			}
			return nil
		},
	})
	require.Error(t, err)
	assert.True(t, res.RolledBack)
	assert.NoError(t, res.RollbackErr)
	assert.Equal(t, 2, activations)
	assert.Equal(t, 2, verifies)
	assert.Contains(t, r.cmds, "sudo cp /etc/a.backup.20260928-010203 /etc/a")
}

func TestApplyVerifyFailsTwiceReportsRollbackErr(t *testing.T) {
	r := &fakeRunner{}
	res, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Activate: func(context.Context, Runner) error { return nil },
		Verify:   func(context.Context, Runner) error { return errors.New("unhealthy") },
	})
	require.Error(t, err)
	assert.True(t, res.RolledBack)
	require.Error(t, res.RollbackErr)
	assert.Contains(t, err.Error(), "unhealthy")
	assert.Contains(t, err.Error(), "rollback")
}

func TestApplyRemovesAFileThatDidNotExistOnRollback(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{out: map[string]string{"sudo sh -c 'test -e /etc/new": "absent"}}
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/new", Content: []byte("x")}},
		Validate: func(context.Context, Runner) error { return errors.New("bad") },
	})
	require.Error(t, err)
	assert.Contains(t, r.cmds, "sudo rm -f /etc/new")
	for _, c := range r.cmds {
		assert.False(t, strings.HasPrefix(c, "sudo cp /etc/new.backup"), "must not restore from a backup that was never taken: %s", c)
	}
}

func TestApplyActivateErrorIsReturnedUnwrappedWithoutVerify(t *testing.T) {
	r := &fakeRunner{}
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Activate: func(context.Context, Runner) error { return errors.New("reload HAProxy: boom") },
	})
	require.EqualError(t, err, "reload HAProxy: boom")
}

func TestApplySetsModeOnlyWhenGiven(t *testing.T) {
	r := &fakeRunner{}
	_, err := Apply(t.Context(), r, Change{Files: []File{
		{Path: "/etc/a", Content: []byte("x")},
		{Path: "/usr/local/libexec/b", Content: []byte("y"), Mode: 0o755},
	}})
	require.NoError(t, err)
	assert.Contains(t, r.cmds, "sudo chmod 755 /usr/local/libexec/b")
	for _, c := range r.cmds {
		assert.NotEqual(t, "sudo chmod 0 /etc/a", c)
		assert.False(t, strings.HasPrefix(c, "sudo chmod") && strings.HasSuffix(c, " /etc/a"))
	}
}

func TestApplyBackupFailureAbortsBeforeInstall(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{fail: map[string]error{"sudo cp -p /etc/a": errors.New("permission denied")}}
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Validate: func(context.Context, Runner) error { return nil },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "back up /etc/a")
	assert.Contains(t, r.cmds, "sudo rm -f /etc/.a.new.20260928-010203")
	// I1 regression guard: a backup failure must never remove or overwrite
	// the live file, since the live file is all that's left of the previous
	// working config.
	assert.NotContains(t, r.cmds, "sudo rm -f /etc/a")
	for _, c := range r.cmds {
		assert.False(t, strings.HasPrefix(c, "sudo mv "), "should not have attempted mv")
	}
}

func TestApplyProbeErrorAbortsBeforeInstall(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{fail: map[string]error{"sudo sh -c 'test -e /etc/a": errors.New("permission denied")}}
	_, err := Apply(t.Context(), r, Change{
		Files:    []File{{Path: "/etc/a", Content: []byte("x")}},
		Validate: func(context.Context, Runner) error { return nil },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "back up /etc/a")
	assert.Contains(t, r.cmds, "sudo rm -f /etc/.a.new.20260928-010203")
	for _, c := range r.cmds {
		assert.False(t, strings.Contains(c, "sudo mv"))
	}
}

func TestApplySecondFileFailureRestoresFirstInReverse(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{fail: map[string]error{"sudo mv /etc/.b.new": errors.New("fail")}}
	_, err := Apply(t.Context(), r, Change{
		Files: []File{
			{Path: "/etc/a", Content: []byte("x")},
			{Path: "/etc/b", Content: []byte("y")},
		},
	})
	require.Error(t, err)

	failIdx := -1
	for i, c := range r.cmds {
		if c == "sudo mv /etc/.b.new.20260928-010203 /etc/b" {
			failIdx = i
		}
	}
	require.NotEqual(t, -1, failIdx, "the failing mv of /etc/b must have been issued")
	// Exact tail: proves cleanup of b's temp file happens exactly once, then
	// both files are restored in reverse order and nothing else runs.
	assert.Equal(t, []string{
		"sudo rm -f /etc/.b.new.20260928-010203",
		"sudo cp /etc/b.backup.20260928-010203 /etc/b",
		"sudo cp /etc/a.backup.20260928-010203 /etc/a",
	}, r.cmds[failIdx+1:])
}

func TestApplyMixedNewAndExistingRollback(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{out: map[string]string{"sudo sh -c 'test -e /etc/b": "absent"}}
	var validated bool
	_, err := Apply(t.Context(), r, Change{
		Files: []File{
			{Path: "/etc/a", Content: []byte("x")},
			{Path: "/etc/b", Content: []byte("y")},
		},
		Validate: func(context.Context, Runner) error { validated = true; return errors.New("bad") },
	})
	require.Error(t, err)
	assert.True(t, validated)

	lastInstallIdx := -1
	for i, c := range r.cmds {
		if c == "sudo mv /etc/.b.new.20260928-010203 /etc/b" {
			lastInstallIdx = i
		}
	}
	require.NotEqual(t, -1, lastInstallIdx, "the install of /etc/b must have succeeded before Validate runs")
	// Exact tail: /etc/b (no backup) is removed, then /etc/a (had a backup)
	// is restored, in reverse install order, and nothing else runs.
	assert.Equal(t, []string{
		"sudo rm -f /etc/b",
		"sudo cp /etc/a.backup.20260928-010203 /etc/a",
	}, r.cmds[lastInstallIdx+1:])
}

func TestApplyRollbackIgnoresCancelledContextDuringVerify(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())

	verifyCount, activateCount := 0, 0
	res, err := Apply(ctx, r, Change{
		Files: []File{{Path: "/etc/a", Content: []byte("x")}},
		// Pinned stubs: if rollBackAfter regressed to passing the caller's
		// (now cancelled) ctx instead of the recover ctx, these would return
		// ctx.Err() on the rollback call and the assertions below would catch it.
		Activate: func(c context.Context, _ Runner) error {
			activateCount++
			return c.Err()
		},
		Verify: func(c context.Context, _ Runner) error {
			verifyCount++
			if verifyCount == 1 {
				cancel()
				return errors.New("unhealthy")
			}
			return c.Err()
		},
	})
	require.Error(t, err)
	assert.True(t, res.RolledBack)
	assert.NoError(t, res.RollbackErr)
	assert.Equal(t, 2, activateCount, "activate should have been called twice: initial and rollback")
	assert.Equal(t, 2, verifyCount, "verify should have been called twice: initial and rollback")
	assert.Contains(t, r.cmds, "sudo cp /etc/a.backup.20260928-010203 /etc/a")
}

func TestApplyRollbackIgnoresCancelledContextDuringValidate(t *testing.T) {
	fixedNow(t)
	r := &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())

	_, err := Apply(ctx, r, Change{
		Files: []File{{Path: "/etc/a", Content: []byte("x")}},
		Validate: func(c context.Context, _ Runner) error {
			cancel()
			return errors.New("bad")
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed (rolled back)")
	assert.Contains(t, r.cmds, "sudo cp /etc/a.backup.20260928-010203 /etc/a")
}

func TestApplyInLoopAbortIgnoresCancelledContext(t *testing.T) {
	fixedNow(t)
	ctx, cancel := context.WithCancel(context.Background())
	// The fake cancels the caller's context the instant it sees the second
	// file's mv, then (since ctx.Err() is now non-nil) fails that same mv --
	// simulating a Ctrl-C racing with an in-flight command.
	r := &fakeRunner{cancelOn: "sudo mv /etc/.b.new", cancelFn: cancel}
	_, err := Apply(ctx, r, Change{
		Files: []File{
			{Path: "/etc/a", Content: []byte("x")},
			{Path: "/etc/b", Content: []byte("y")},
		},
	})
	require.Error(t, err)
	// No restore-failure text appended: the cleanup and restore below all
	// succeeded even though the caller's ctx was already cancelled.
	assert.Equal(t, "install config: context canceled", err.Error())
	assert.Contains(t, r.cmds, "sudo rm -f /etc/.b.new.20260928-010203")
	assert.Contains(t, r.cmds, "sudo cp /etc/b.backup.20260928-010203 /etc/b")
	assert.Contains(t, r.cmds, "sudo cp /etc/a.backup.20260928-010203 /etc/a")
}

func TestApplyRejectsUnsafePath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"path with space", "/etc/a b"},
		{"path with semicolon", "/etc/a;rm"},
		{"path with backtick", "/etc/a`x`"},
		{"relative path", "etc/a"},
		{"no leading slash", "a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{}
			_, err := Apply(t.Context(), r, Change{
				Files: []File{{Path: tt.path, Content: []byte("x")}},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "reject unsafe path")
		})
	}
}

func TestUnchangedComparesHashes(t *testing.T) {
	sum := "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" // sha256("x")
	r := &fakeRunner{out: map[string]string{"sha256sum": sum + "  /etc/a\n"}}
	same, err := Unchanged(t.Context(), r, []File{{Path: "/etc/a", Content: []byte("x")}})
	require.NoError(t, err)
	assert.True(t, same)

	same, err = Unchanged(t.Context(), r, []File{{Path: "/etc/a", Content: []byte("changed")}})
	require.NoError(t, err)
	assert.False(t, same)
}

func TestUnchangedMissingFile(t *testing.T) {
	r := &fakeRunner{out: map[string]string{"sudo sh -c 'if test -e": "missing"}}
	same, err := Unchanged(t.Context(), r, []File{{Path: "/etc/a", Content: []byte("x")}})
	require.NoError(t, err)
	assert.False(t, same)
}

func TestUnchangedReturnsRunnerError(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"sudo sh -c 'if test -e": errors.New("permission denied")}}
	same, err := Unchanged(t.Context(), r, []File{{Path: "/etc/a", Content: []byte("x")}})
	require.Error(t, err)
	assert.False(t, same)
}

func TestUnchangedRejectsUnsafePath(t *testing.T) {
	r := &fakeRunner{}
	_, err := Unchanged(t.Context(), r, []File{{Path: "/etc/a b", Content: []byte("x")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reject unsafe path")
}
