package haproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// sshRunner defines the interface for SSH operations
type sshRunner interface {
	runSSH(cmd string) error
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
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	authMethods := []ssh.AuthMethod{ssh.PublicKeys(signer)}

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
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers)
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
	addr := net.JoinHostPort(c.sshHost, c.sshPort)
	conn, err := ssh.Dial("tcp", addr, c.sshConfig)
	if err != nil {
		return fmt.Errorf("dial SSH: %w", err)
	}
	defer func() { _ = conn.Close() }()

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("create SSH session: %w", err)
	}
	defer func() { _ = session.Close() }()

	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("run SSH command: %w, output: %s", err, string(output))
	}

	return nil
}

// knownHostsCallback returns an ssh.HostKeyCallback. When insecure is true,
// all host keys are accepted. Otherwise, keys are verified against the user's
// ~/.ssh/known_hosts file with trust-on-first-use (TOFU): unknown keys are
// automatically added to known_hosts, while mismatched keys are rejected.
func knownHostsCallback(insecure bool) ssh.HostKeyCallback {
	if insecure {
		return ssh.InsecureIgnoreHostKey()
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("SSH host key verification failed for %s: cannot determine home directory: %v", hostname, err)
		}
	}

	khPath := filepath.Join(home, ".ssh", "known_hosts")

	// Ensure ~/.ssh directory and known_hosts file exist
	sshDir := filepath.Dir(khPath)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("SSH host key verification failed: cannot create %s: %v", sshDir, err)
		}
	}
	if _, err := os.Stat(khPath); os.IsNotExist(err) {
		if err := os.WriteFile(khPath, nil, 0600); err != nil {
			return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
				return fmt.Errorf("SSH host key verification failed: cannot create %s: %v", khPath, err)
			}
		}
	}

	cb, err := knownhosts.New(khPath)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("SSH host key verification failed for %s: cannot read known_hosts: %v", hostname, err)
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err == nil {
			return nil
		}

		// Check if this is a KeyError (unknown or mismatch)
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}

		// If Want is non-empty, the file has a different key for this host - reject (mismatch)
		if len(keyErr.Want) > 0 {
			return fmt.Errorf("SSH host key mismatch for %s: the server key has changed. Remove the old key with: ssh-keygen -R %s", hostname, hostname)
		}

		// Want is empty - key is unknown. Trust on first use: append to known_hosts.
		return appendKnownHost(khPath, remote, key)
	}
}

// appendKnownHost adds a new host key entry to the known_hosts file.
func appendKnownHost(khPath string, remote net.Addr, key ssh.PublicKey) error {
	// knownhosts.Normalize gives us the right format (e.g. "[host]:port" or just "host")
	host := knownhosts.Normalize(remote.String())
	line := knownhosts.Line([]string{host}, key)

	f, err := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to add host key to known_hosts: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("failed to write host key to known_hosts: %w", err)
	}

	return nil
}
