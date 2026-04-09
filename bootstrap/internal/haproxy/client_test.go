package haproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/crypto/ssh"
)

// mockSSHServer is a test SSH server that responds to commands
type mockSSHServer struct {
	listener  net.Listener
	hostKey   ssh.Signer
	commands  []string
	mu        sync.Mutex
	responses map[string]struct {
		output     string
		exitStatus uint32
	}
	failNext error
}

func newMockSSHServer(t *testing.T) *mockSSHServer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &mockSSHServer{
		listener: listener,
		hostKey:  signer,
		commands: make([]string, 0),
		responses: make(map[string]struct {
			output     string
			exitStatus uint32
		}),
	}

	go server.serve(t)
	time.Sleep(10 * time.Millisecond) // Give server time to start
	return server
}

func (s *mockSSHServer) serve(t *testing.T) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		go s.handleConn(t, conn)
	}
}

func (s *mockSSHServer) handleConn(_ *testing.T, netConn net.Conn) {
	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(s.hostKey)

	_, chans, reqs, err := ssh.NewServerConn(netConn, config)
	if err != nil {
		return
	}

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}

		go func() {
			defer func() { _ = channel.Close() }()

			for req := range requests {
				switch req.Type {
				case "exec":
					if len(req.Payload) >= 4 {
						// Decode uint32 length (big-endian) from first 4 bytes
						cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
						if cmdLen > 0 && cmdLen < 100000 && len(req.Payload) >= 4+cmdLen {
							cmd := string(req.Payload[4 : 4+cmdLen])

							s.mu.Lock()
							s.commands = append(s.commands, cmd)
							response, exists := s.responses[cmd]
							if !exists {
								// Try prefix matching for commands with dynamic parts
								for pattern, resp := range s.responses {
									if len(cmd) >= len(pattern) && cmd[:len(pattern)] == pattern {
										response = resp
										exists = true
										break
									}
								}
							}
							failNow := s.failNext
							s.failNext = nil
							s.mu.Unlock()

							_ = req.Reply(true, nil)

							if failNow != nil {
								_, _ = channel.Write([]byte(failNow.Error() + "\n"))
								_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
								return // Exit after handling exec
							} else {
								if exists {
									_, _ = channel.Write([]byte(response.output))
									_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{response.exitStatus}))
								} else {
									// Default success response
									_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
								}
								return // Exit after handling exec
							}
						}
					}
				case "subsystem":
					_ = req.Reply(false, nil)
				default:
					_ = req.Reply(req.WantReply, nil)
				}
			}
		}()
	}
}

func (s *mockSSHServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *mockSSHServer) Host() string {
	host, _, _ := net.SplitHostPort(s.Addr())
	return host
}

func (s *mockSSHServer) Port() int {
	_, portStr, _ := net.SplitHostPort(s.Addr())
	port, _ := strconv.Atoi(portStr)
	return port
}

func (s *mockSSHServer) Close() {
	_ = s.listener.Close()
}

func (s *mockSSHServer) SetResponse(cmd, response string, exitStatus uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[cmd] = struct {
		output     string
		exitStatus uint32
	}{response, exitStatus}
}

func (s *mockSSHServer) SetNextError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

func (s *mockSSHServer) GetCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmds := make([]string, len(s.commands))
	copy(cmds, s.commands)
	return cmds
}

func generateTestKey(t *testing.T) (string, ssh.Signer) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	tmpFile, err := os.CreateTemp("", "test_key_*")
	require.NoError(t, err)
	defer func() { _ = tmpFile.Close() }()

	_, err = tmpFile.Write(privateKeyPEM)
	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)

	return tmpFile.Name(), signer
}

// mockRunner implements sshRunner for testing
type mockRunner struct {
	server *mockSSHServer
	t      *testing.T
}

func (m *mockRunner) runSSH(cmd string) error {
	// Connect to mock server instead of real SSH
	host := m.server.Host()
	port := m.server.Port()

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	// Create SSH config for mock server (no auth needed)
	config := &ssh.ClientConfig{
		User:            "admin",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	conn, err := ssh.Dial("tcp", addr, config)
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

// createTestClient creates a client that uses the mock SSH server
func createTestClient(t *testing.T, server *mockSSHServer) *Client {
	logger := zaptest.NewLogger(t)
	host := server.Host()
	client := NewClient("admin", host, logger, true)
	// Replace the runner with our mock
	client.runner = &mockRunner{server: server, t: t}
	return client
}

func TestNewClient(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tests := []struct {
		name    string
		sshUser string
		sshHost string
	}{
		{
			name:    "standard client",
			sshUser: "admin",
			sshHost: "192.168.1.10",
		},
		{
			name:    "client with IPv6",
			sshUser: "root",
			sshHost: "::1",
		},
		{
			name:    "client with hostname",
			sshUser: "user",
			sshHost: "haproxy.example.com",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := NewClient(tt.sshUser, tt.sshHost, logger, true)

			assert.NotNil(t, client)
			assert.Equal(t, tt.sshUser, client.sshUser)
			assert.Equal(t, tt.sshHost, client.sshHost)
			assert.NotNil(t, client.sshConfig)
			assert.Equal(t, 10*time.Second, client.sshConfig.Timeout)
			assert.NotNil(t, client.logger)
			assert.NotNil(t, client.runner)
			assert.Equal(t, "22", client.sshPort)
		})
	}
}

func TestSetPrivateKey(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewClient("admin", "192.168.1.10", logger, true)

	t.Run("non-existent key", func(t *testing.T) {
		err := client.SetPrivateKey("/nonexistent/key")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "read private key")
	})

	t.Run("invalid key content", func(t *testing.T) {
		tmpFile := t.TempDir() + "/test_key"
		err := os.WriteFile(tmpFile, []byte("invalid key data"), 0600)
		require.NoError(t, err)

		err = client.SetPrivateKey(tmpFile)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parse private key")
	})

	t.Run("valid key", func(t *testing.T) {
		keyPath, _ := generateTestKey(t)
		err := client.SetPrivateKey(keyPath)
		assert.NoError(t, err)
		// At least 1 (key auth), possibly 2 if SSH agent is available
		assert.GreaterOrEqual(t, len(client.sshConfig.Auth), 1)
	})

	t.Run("permission denied on key file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Skipping permission test on Windows - file permissions are not enforced the same way")
		}
		if os.Getuid() == 0 {
			t.Skip("Skipping permission test when running as root")
		}

		tmpFile := t.TempDir() + "/noperm_key"
		err := os.WriteFile(tmpFile, []byte("test"), 0000)
		require.NoError(t, err)

		err = client.SetPrivateKey(tmpFile)
		assert.Error(t, err)
	})
}

func TestSetPort(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewClient("admin", "192.168.1.10", logger, true)

	assert.Equal(t, "22", client.sshPort)
	client.SetPort("2222")
	assert.Equal(t, "2222", client.sshPort)
}

func TestClient_Update_Success(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()

	client := createTestClient(t, server)

	// Set successful responses for all commands
	server.SetResponse("echo", "", 0) // base64 decode command prefix
	server.SetResponse("sudo cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.backup.", "", 0)
	server.SetResponse("sudo mv /tmp/haproxy.cfg.new /etc/haproxy/haproxy.cfg", "", 0)
	server.SetResponse("sudo haproxy -c -f /etc/haproxy/haproxy.cfg", "Configuration file is valid\n", 0)
	server.SetResponse("sudo systemctl reload haproxy", "", 0)

	ctx := context.Background()
	config := "test config"

	err := client.Update(ctx, config)
	assert.NoError(t, err)

	commands := server.GetCommands()
	assert.GreaterOrEqual(t, len(commands), 4, "expected at least 4 commands, got %d", len(commands))
	// First command should be the base64 decode
	assert.Contains(t, commands[0], "base64 -d")
}

func TestClient_Update_ValidationFails(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()

	client := createTestClient(t, server)

	server.SetResponse("echo", "", 0)
	server.SetResponse("sudo cp", "", 0)
	server.SetResponse("sudo mv /tmp/haproxy.cfg.new /etc/haproxy/haproxy.cfg", "", 0)
	server.SetResponse("sudo haproxy -c -f /etc/haproxy/haproxy.cfg", "[ALERT] Configuration invalid\n", 1)
	server.SetResponse("sudo cp /etc/haproxy/haproxy.cfg.backup.", "", 0)

	ctx := context.Background()
	config := "invalid config"

	err := client.Update(ctx, config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed")
}

func TestClient_Update_RollbackAlsoFails(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()

	client := createTestClient(t, server)

	server.SetResponse("echo", "", 0)
	// Use a pattern that distinguishes backup from rollback - backup copies FROM haproxy.cfg, rollback copies TO haproxy.cfg
	server.SetResponse("sudo cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.backup.", "", 0) // backup succeeds
	server.SetResponse("sudo mv /tmp/haproxy.cfg.new /etc/haproxy/haproxy.cfg", "", 0)
	server.SetResponse("sudo haproxy -c -f /etc/haproxy/haproxy.cfg", "[ALERT] Configuration invalid\n", 1)
	server.SetResponse("sudo cp /etc/haproxy/haproxy.cfg.backup.", "cp: cannot stat", 1) // rollback fails

	ctx := context.Background()
	config := "bad config"

	err := client.Update(ctx, config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed and rollback also failed")
}

func TestClient_Update_WriteFails(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()

	client := createTestClient(t, server)
	server.SetNextError(fmt.Errorf("permission denied"))

	ctx := context.Background()
	config := "test config"

	err := client.Update(ctx, config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "write temp config")
}

func TestClient_Validate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := newMockSSHServer(t)
		defer server.Close()

		client := createTestClient(t, server)
		server.SetResponse("sudo systemctl is-active haproxy", "active\n", 0)

		ctx := context.Background()
		err := client.Validate(ctx)
		assert.NoError(t, err)
	})

	t.Run("not active", func(t *testing.T) {
		server := newMockSSHServer(t)
		defer server.Close()

		client := createTestClient(t, server)
		server.SetResponse("sudo systemctl is-active haproxy", "inactive\n", 3)

		ctx := context.Background()
		err := client.Validate(ctx)
		assert.Error(t, err)
	})

	t.Run("connection refused", func(t *testing.T) {
		logger := zaptest.NewLogger(t)
		client := NewClient("admin", "127.0.0.1", logger, true)
		// Use a port that's unlikely to be open
		client.SetPort("1")

		ctx := context.Background()
		err := client.Validate(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "dial SSH")
	})
}

func TestBase64Encode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "aGVsbG8="},
		{"", ""},
		{"test config\nwith newlines", "dGVzdCBjb25maWcKd2l0aCBuZXdsaW5lcw=="},
		{"special chars: !@#$%^&*()", "c3BlY2lhbCBjaGFyczogIUAjJCVeJiooKQ=="},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result := base64Encode(tt.input)
			assert.Equal(t, tt.expected, result)

			// Verify it's reversible
			decoded, err := base64Decode(result)
			require.NoError(t, err)
			assert.Equal(t, tt.input, decoded)
		})
	}
}

// Helper to verify base64 encoding
func base64Decode(s string) (string, error) {
	bytes, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// Integration test (skipped by default)
func TestClient_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	sshHost := os.Getenv("TEST_HAPROXY_HOST")
	sshUser := os.Getenv("TEST_HAPROXY_USER")
	keyPath := os.Getenv("TEST_SSH_KEY_PATH")

	if sshHost == "" || sshUser == "" {
		t.Skip("Set TEST_HAPROXY_HOST, TEST_HAPROXY_USER for integration tests")
	}

	logger := zaptest.NewLogger(t)
	client := NewClient(sshUser, sshHost, logger, true)

	if keyPath != "" {
		err := client.SetPrivateKey(keyPath)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test validation
	err := client.Validate(ctx)
	if err != nil {
		t.Logf("Validate error (may be expected if HAProxy not running): %v", err)
	}

	// Test config update with validation that will fail (and rollback)
	config := `global
    maxconn 4096

defaults
    mode tcp
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend test
    bind *:8080
`

	err = client.Update(ctx, config)
	if err != nil {
		t.Logf("Update error (may be expected): %v", err)
	}
}

func TestKnownHostsCallback_Insecure(t *testing.T) {
	cb := knownHostsCallback(true)
	// Should accept any key without error
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubKey, err := ssh.NewPublicKey(&key.PublicKey)
	require.NoError(t, err)

	addr := &net.TCPAddr{IP: net.ParseIP("192.168.1.100"), Port: 22}
	err = cb("192.168.1.100:22", addr, pubKey)
	assert.NoError(t, err)
}

func TestKnownHostsCallback_TOFU(t *testing.T) {
	// Create a temp known_hosts file via temp HOME
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmpHome)
	}

	sshDir := filepath.Join(tmpHome, ".ssh")
	require.NoError(t, os.Mkdir(sshDir, 0700))

	cb := knownHostsCallback(false)

	// Generate a host key
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubKey, err := ssh.NewPublicKey(&key.PublicKey)
	require.NoError(t, err)

	addr := &net.TCPAddr{IP: net.ParseIP("192.168.1.100"), Port: 22}

	// First connection: unknown key should be accepted (TOFU) and written to known_hosts
	err = cb("192.168.1.100:22", addr, pubKey)
	assert.NoError(t, err)

	// Verify the key was written to known_hosts
	khPath := filepath.Join(sshDir, "known_hosts")
	data, err := os.ReadFile(khPath)
	require.NoError(t, err)
	assert.NotEmpty(t, data, "known_hosts should contain the new key")
}

func TestKnownHostsCallback_Mismatch(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmpHome)
	}

	sshDir := filepath.Join(tmpHome, ".ssh")
	require.NoError(t, os.Mkdir(sshDir, 0700))

	// First, establish a known key via TOFU
	cb := knownHostsCallback(false)

	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubKey1, err := ssh.NewPublicKey(&key1.PublicKey)
	require.NoError(t, err)

	addr := &net.TCPAddr{IP: net.ParseIP("192.168.1.100"), Port: 22}
	err = cb("192.168.1.100:22", addr, pubKey1)
	assert.NoError(t, err)

	// Re-create the callback to reload known_hosts with the new entry
	cb = knownHostsCallback(false)

	// Now try with a different key - should be rejected as mismatch
	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubKey2, err := ssh.NewPublicKey(&key2.PublicKey)
	require.NoError(t, err)

	err = cb("192.168.1.100:22", addr, pubKey2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mismatch")
}

// Benchmarks
func BenchmarkBase64Encode(b *testing.B) {
	config := "global\n    maxconn 32000\n\ndefaults\n    mode tcp\n"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = base64Encode(config)
	}
}
