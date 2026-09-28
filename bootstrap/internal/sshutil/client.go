package sshutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// KnownHostsCallback returns an ssh.HostKeyCallback. When insecure is true,
// all host keys are accepted. Otherwise, keys are verified against the user's
// ~/.ssh/known_hosts file with trust-on-first-use (TOFU): unknown keys are
// automatically added to known_hosts, while mismatched keys are rejected.
func KnownHostsCallback(insecure bool) ssh.HostKeyCallback {
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

// PrivateKeyAuth loads and parses an SSH private key file into an auth method.
func PrivateKeyAuth(keyPath string) (ssh.AuthMethod, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	return ssh.PublicKeys(signer), nil
}

// AgentAuth returns an ssh.AuthMethod using the SSH agent, or nil if unavailable.
func AgentAuth() ssh.AuthMethod {
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

// Run dials addr and runs cmd, returning its combined output. Output is
// returned alongside the error when the command ran but exited non-zero, so a
// caller that can interpret the output does not lose it.
func Run(addr string, cfg *ssh.ClientConfig, cmd string) (string, error) {
	return RunContext(context.Background(), addr, cfg, cmd)
}

// RunContext is Run bounded by ctx. The SSH library takes no context, so a
// cancelled ctx closes the connection underneath the handshake or the
// running command, which is the only way to unblock either; the error then
// wraps ctx.Err() so a caller can tell an interrupt from a failed command.
func RunContext(ctx context.Context, addr string, cfg *ssh.ClientConfig, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("run SSH command: %w", err)
	}

	d := net.Dialer{Timeout: cfg.Timeout}
	netConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial SSH: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = netConn.Close() })
	defer stop()

	c, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		_ = netConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("dial SSH: %w", ctxErr)
		}
		return "", fmt.Errorf("dial SSH: %w", err)
	}
	conn := ssh.NewClient(c, chans, reqs)
	defer func() { _ = conn.Close() }()

	session, err := conn.NewSession()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("create SSH session: %w", ctxErr)
		}
		return "", fmt.Errorf("create SSH session: %w", err)
	}
	defer func() { _ = session.Close() }()

	output, err := session.CombinedOutput(cmd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return string(output), fmt.Errorf("run SSH command: %w, output: %s", ctxErr, string(output))
		}
		return string(output), fmt.Errorf("run SSH command: %w, output: %s", err, string(output))
	}

	return string(output), nil
}

// Client runs commands on one host over SSH, trusting it through known_hosts
// the same way every talops caller does.
type Client struct {
	user, host, port string
	cfg              ssh.ClientConfig
}

func NewClient(user, host string, insecure bool) *Client {
	return &Client{user: user, host: host, port: "22", cfg: ssh.ClientConfig{
		User:            user,
		HostKeyCallback: KnownHostsCallback(insecure),
		Timeout:         10 * time.Second,
	}}
}

func (c *Client) UseKey(path string) error {
	auth, err := PrivateKeyAuth(path)
	if err != nil {
		return err
	}
	c.cfg.Auth = append(c.cfg.Auth, auth)
	return nil
}

func (c *Client) UseAgent() bool {
	auth := AgentAuth()
	if auth == nil {
		return false
	}
	c.cfg.Auth = append(c.cfg.Auth, auth)
	return true
}

func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	addr := net.JoinHostPort(c.host, c.port)
	cfg := c.cfg
	cfg.HostKeyAlgorithms = HostKeyAlgorithms(addr)
	return RunContext(ctx, addr, &cfg, cmd)
}
