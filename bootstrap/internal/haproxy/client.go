package haproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/sshutil"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// sshRunner defines the interface for SSH operations.
//
// Read commands need the remote stdout, and they need it even when the command
// exits non-zero: `systemctl is-active` reports "inactive" on stdout and exits
// 3, so discarding output on error would turn a known state into an unknown
// one.
type sshRunner interface {
	runSSH(cmd string) error
	runSSHOutput(cmd string) (string, error)
}

// Client manages HAProxy configuration via SSH
type Client struct {
	sshUser   string
	sshHost   string
	sshPort   string
	sshConfig *ssh.ClientConfig
	logger    *zap.Logger
	runner    sshRunner // injectable for testing
}

// NewClient creates a new HAProxy SSH client.
// If insecureSSH is false, host keys are verified against ~/.ssh/known_hosts.
func NewClient(sshUser, sshHost string, logger *zap.Logger, insecureSSH bool) *Client {
	hostKeyCallback := knownHostsCallback(insecureSSH)

	c := &Client{
		sshUser: sshUser,
		sshHost: sshHost,
		sshPort: "22",
		logger:  logger,
		sshConfig: &ssh.ClientConfig{
			User:            sshUser,
			HostKeyCallback: hostKeyCallback,
			Timeout:         10 * time.Second,
		},
	}
	c.runner = c // default runner is self
	return c
}

// SetPrivateKey configures SSH public key authentication.
// It also appends SSH agent auth as a fallback if SSH_AUTH_SOCK is available.
func (c *Client) SetPrivateKey(keyPath string) error {
	auth, err := sshutil.PrivateKeyAuth(keyPath)
	if err != nil {
		return err
	}

	authMethods := []ssh.AuthMethod{auth}

	// Append SSH agent as fallback if available
	if agentAuth := sshAgentAuth(); agentAuth != nil {
		authMethods = append(authMethods, agentAuth)
	}
	c.sshConfig.Auth = authMethods
	return nil
}

// SetSSHAgent configures SSH agent authentication only (no key file).
// Use when no explicit key path is provided but SSH_AUTH_SOCK is available.
func (c *Client) SetSSHAgent() bool {
	if agentAuth := sshAgentAuth(); agentAuth != nil {
		c.sshConfig.Auth = []ssh.AuthMethod{agentAuth}
		return true
	}
	return false
}

// sshAgentAuth returns an ssh.AuthMethod using the SSH agent, or nil if unavailable.
func sshAgentAuth() ssh.AuthMethod {
	return sshutil.AgentAuth()
}

// SetPort allows overriding the default SSH port (for testing)
func (c *Client) SetPort(port string) {
	c.sshPort = port
}

// Update writes a new HAProxy configuration, validates it, and reloads the service.
// On validation failure, it automatically rolls back to the previous config.
// Retries up to maxRetries times on SSH connection failures before giving up.
func (c *Client) Update(ctx context.Context, config string) error {
	return c.UpdateWithRetry(ctx, config, 3)
}

// UpdateWithRetry is like Update but allows specifying the retry count.
func (c *Client) UpdateWithRetry(ctx context.Context, config string, maxRetries int) error {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			c.logger.Info("retrying HAProxy update",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxRetries),
				zap.Error(lastErr))
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during HAProxy retry: %w", ctx.Err())
			case <-time.After(5 * time.Second):
			}
		}

		lastErr = c.doUpdate(ctx, config)
		if lastErr == nil {
			return nil
		}

		// Only retry on SSH connection errors, not config validation failures
		if isSSHConnectionError(lastErr) {
			continue
		}
		return lastErr
	}
	return fmt.Errorf("HAProxy update failed after %d attempts: %w", maxRetries, lastErr)
}

// isSSHConnectionError returns true if the error is an SSH dial/connection failure
// (as opposed to a command execution or validation failure).
func isSSHConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, substr := range []string{"dial SSH", "unable to authenticate", "connection refused", "i/o timeout", "connection reset"} {
		if contains(msg, substr) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (c *Client) doUpdate(_ context.Context, config string) error {
	timestamp := time.Now().Format("20060102-150405")

	c.logger.Info("updating HAProxy configuration",
		zap.String("host", c.sshHost),
		zap.String("user", c.sshUser),
		zap.String("backup_suffix", timestamp))

	// 1. Write new config to temp location using base64 to avoid heredoc injection
	encoded := base64Encode(config)
	writeCmd := fmt.Sprintf("echo '%s' | base64 -d > /tmp/haproxy.cfg.new", encoded)
	if err := c.runner.runSSH(writeCmd); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}

	// 2. Backup existing config
	backupCmd := fmt.Sprintf("sudo cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.backup.%s", timestamp)
	if err := c.runner.runSSH(backupCmd); err != nil {
		c.logger.Warn("failed to backup existing config (may not exist yet)", zap.Error(err))
	}

	// 3. Install new config
	if err := c.runner.runSSH("sudo mv /tmp/haproxy.cfg.new /etc/haproxy/haproxy.cfg"); err != nil {
		return fmt.Errorf("install config: %w", err)
	}

	// 4. Validate config
	if err := c.runner.runSSH("sudo haproxy -c -f /etc/haproxy/haproxy.cfg"); err != nil {
		c.logger.Error("HAProxy config validation failed, rolling back", zap.Error(err))
		rollbackCmd := fmt.Sprintf("sudo cp /etc/haproxy/haproxy.cfg.backup.%s /etc/haproxy/haproxy.cfg", timestamp)
		if rollbackErr := c.runner.runSSH(rollbackCmd); rollbackErr != nil {
			return fmt.Errorf("config validation failed and rollback also failed: validation=%w, rollback=%v", err, rollbackErr)
		}
		return fmt.Errorf("config validation failed (rolled back): %w", err)
	}

	// 5. Reload HAProxy
	if err := c.runner.runSSH("sudo systemctl reload haproxy"); err != nil {
		return fmt.Errorf("reload HAProxy: %w", err)
	}

	c.logger.Info("HAProxy configuration updated and reloaded successfully")
	return nil
}

// Validate checks if HAProxy is currently running and healthy
func (c *Client) Validate(_ context.Context) error {
	return c.runner.runSSH("sudo systemctl is-active haproxy")
}

// CheckConnectivity verifies SSH connectivity to the HAProxy host.
// Returns nil if SSH connection succeeds, or an error describing the failure.
func (c *Client) CheckConnectivity() error {
	return c.runner.runSSH("echo ok")
}

func (c *Client) runSSH(cmd string) error {
	_, err := c.runSSHOutput(cmd)
	return err
}

// runSSHOutput runs a command and returns its combined output. Output is
// returned alongside the error when the command ran but exited non-zero, so a
// caller that can interpret the output does not lose it.
func (c *Client) runSSHOutput(cmd string) (string, error) {
	addr := net.JoinHostPort(c.sshHost, c.sshPort)

	// Resolved here rather than in NewClient so a keyscan performed between
	// construction and the dial is picked up.
	cfg := *c.sshConfig
	cfg.HostKeyAlgorithms = sshutil.HostKeyAlgorithms(addr)

	return sshutil.Run(addr, &cfg, cmd)
}

// knownHostsCallback returns an ssh.HostKeyCallback. When insecure is true,
// all host keys are accepted. Otherwise, keys are verified against the user's
// ~/.ssh/known_hosts file with trust-on-first-use (TOFU): unknown keys are
// automatically added to known_hosts, while mismatched keys are rejected.
func knownHostsCallback(insecure bool) ssh.HostKeyCallback {
	return sshutil.KnownHostsCallback(insecure)
}
