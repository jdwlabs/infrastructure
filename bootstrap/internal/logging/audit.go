package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// AuditLogger records command executions and state changes to an audit trail.
// Uses io.Writer so tests can inject bytes.Buffer.
type AuditLogger struct {
	w io.Writer
}

// NewAuditLogger creates an audit logger writing to w.
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{w: w}
}

// WriteEntry writes a tagged audit log entry.
func (a *AuditLogger) WriteEntry(tag, message string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	_, _ = fmt.Fprintf(a.w, "[%s] [%s] %s\n", ts, tag, message)
}

// Command creates an AuditedCmd that wraps exec.Cmd with automatic audit logging.
func (a *AuditLogger) Command(name string, args ...string) *AuditedCmd {
	return &AuditedCmd{
		Cmd:   exec.Command(name, args...),
		audit: a,
		name:  name,
		args:  args,
	}
}

// CommandContext creates an AuditedCmd with a context for cancellation/timeout support.
func (a *AuditLogger) CommandContext(ctx context.Context, name string, args ...string) *AuditedCmd {
	return &AuditedCmd{
		Cmd:   exec.CommandContext(ctx, name, args...),
		audit: a,
		name:  name,
		args:  args,
	}
}

// AuditedCmd wraps exec.Cmd to automatically record command lifecycle to the audit log.
type AuditedCmd struct {
	*exec.Cmd
	audit *AuditLogger
	name  string
	args  []string
}

func (c *AuditedCmd) cmdString() string {
	return c.name + " " + strings.Join(c.args, " ")
}

// Run executes the command and records the full lifecycle to the audit log.
func (c *AuditedCmd) Run() error {
	c.audit.WriteEntry("CMD-START", c.cmdString())
	if c.Dir != "" {
		c.audit.WriteEntry("CMD-WD", c.Dir)
	}

	start := time.Now()
	err := c.Cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	c.audit.WriteEntry("CMD-EXIT", fmt.Sprintf("%d [DURATION: %dms]", exitCode, duration.Milliseconds()))
	return err
}

// Output executes the command, captures stdout, and records the lifecycle.
func (c *AuditedCmd) Output() ([]byte, error) {
	c.audit.WriteEntry("CMD-START", c.cmdString())
	if c.Dir != "" {
		c.audit.WriteEntry("CMD-WD", c.Dir)
	}

	start := time.Now()
	out, err := c.Cmd.Output()
	duration := time.Since(start)

	if len(out) > 0 {
		c.audit.WriteEntry("CMD-OUTPUT", "\n"+string(out))
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	c.audit.WriteEntry("CMD-EXIT", fmt.Sprintf("%d [DURATION: %dms]", exitCode, duration.Milliseconds()))
	return out, err
}

// CombinedOutput executes the command, captures stdout+stderr, and records the lifecycle.
func (c *AuditedCmd) CombinedOutput() ([]byte, error) {
	c.audit.WriteEntry("CMD-START", c.cmdString())
	if c.Dir != "" {
		c.audit.WriteEntry("CMD-WD", c.Dir)
	}

	start := time.Now()
	out, err := c.Cmd.CombinedOutput()
	duration := time.Since(start)

	if len(out) > 0 {
		c.audit.WriteEntry("CMD-OUTPUT", "\n"+string(out))
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	c.audit.WriteEntry("CMD-EXIT", fmt.Sprintf("%d [DURATION: %dms]", exitCode, duration.Milliseconds()))
	return out, err
}
