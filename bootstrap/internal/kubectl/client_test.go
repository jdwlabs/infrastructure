package kubectl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// mockExecCmd creates a mock exec.Cmd that returns predefined output
func mockExecCmd(output string, exitCode int) *exec.Cmd {
	if runtime.GOOS == "windows" {
		// Use PowerShell with [Console]::Write which outputs raw text
		// Escape single quotes by doubling them
		escaped := strings.ReplaceAll(output, "'", "''")
		psCmd := fmt.Sprintf("[Console]::Write('%s'); exit %d", escaped, exitCode)
		return exec.Command("powershell", "-Command", psCmd)
	}
	// Unix: use printf for reliable output without trailing newline
	return exec.Command("printf", "%s", output)
}

// Helper to create a test client
func newTestClient(t *testing.T) (*Client, *zap.Logger) {
	logger := zaptest.NewLogger(t)
	return NewClient(logger), logger
}

// Helper to create a test IP
func mustParseIP(ip string) net.IP {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		panic(fmt.Sprintf("invalid IP: %s", ip))
	}
	return parsed
}

func TestNewClient(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewClient(logger)

	require.NotNil(t, client)
	assert.Equal(t, logger, client.logger)
}

func TestClient_SetContext(t *testing.T) {
	client, _ := newTestClient(t)

	assert.Empty(t, client.context)
	client.SetContext("test-context")
	assert.Equal(t, "test-context", client.context)

	args := client.baseArgs()
	assert.Contains(t, args, "--context")
	assert.Contains(t, args, "test-context")
}

func TestClient_SetAuditLogger(t *testing.T) {
	client, _ := newTestClient(t)
	assert.Nil(t, client.audit)

	var buf bytes.Buffer
	audit := logging.NewAuditLogger(&buf)
	client.SetAuditLogger(audit)
	assert.NotNil(t, client.audit)
}

func TestClient_AuditLogging(t *testing.T) {
	var buf bytes.Buffer
	audit := logging.NewAuditLogger(&buf)

	client, _ := newTestClient(t)
	client.SetContext("test-cluster")
	client.SetAuditLogger(audit)

	ctx := context.Background()
	// Command will fail (kubectl not available in test env), but audit should still log
	_, _ = client.GetNodeNameByIP(ctx, mustParseIP("192.168.1.10"))

	auditOutput := buf.String()
	assert.Contains(t, auditOutput, "CMD-START")
	assert.Contains(t, auditOutput, "--context")
	assert.Contains(t, auditOutput, "test-cluster")
	assert.Contains(t, auditOutput, "get nodes")
	assert.Contains(t, auditOutput, "CMD-EXIT")
}

func TestClient_ErrorIncludesFullCommand(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit", "1")
		}
		return exec.Command("false")
	}

	client, _ := newTestClient(t)
	client.SetContext("my-cluster")
	ctx := context.Background()

	_, err := client.GetNodeNameByIP(ctx, mustParseIP("192.168.1.1"))
	assert.Error(t, err)
	// Error should contain the full command with context flag
	assert.Contains(t, err.Error(), "--context")
	assert.Contains(t, err.Error(), "my-cluster")
	assert.Contains(t, err.Error(), "get nodes")
}

func TestClient_baseArgs(t *testing.T) {
	t.Run("empty client", func(t *testing.T) {
		client, _ := newTestClient(t)
		args := client.baseArgs()
		assert.Empty(t, args)
	})

	t.Run("with kubeconfig", func(t *testing.T) {
		client, _ := newTestClient(t)
		client.kubeconfig = "/path/to/kubeconfig"
		args := client.baseArgs()
		assert.Equal(t, []string{"--kubeconfig", "/path/to/kubeconfig"}, args)
	})

	t.Run("with context", func(t *testing.T) {
		client, _ := newTestClient(t)
		client.SetContext("prod-cluster")
		args := client.baseArgs()
		assert.Equal(t, []string{"--context", "prod-cluster"}, args)
	})

	t.Run("with both", func(t *testing.T) {
		client, _ := newTestClient(t)
		client.kubeconfig = "/path/to/kubeconfig"
		client.SetContext("prod-cluster")
		args := client.baseArgs()
		assert.Equal(t, []string{"--kubeconfig", "/path/to/kubeconfig", "--context", "prod-cluster"}, args)
	})
}

func TestClient_GetNodeNameByIP(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	tests := []struct {
		name        string
		mockOutput  string
		mockErr     error
		ip          net.IP
		wantNode    string
		wantErr     bool
		errContains string
	}{
		{
			name:       "node found by IP - control plane",
			mockOutput: "node-1   Ready    control-plane   5d    v1.28.0   192.168.1.10   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0",
			ip:         mustParseIP("192.168.1.10"),
			wantNode:   "node-1",
			wantErr:    false,
		},
		{
			name: "node found by IP - worker",
			mockOutput: "node-1   Ready    control-plane   5d    v1.28.0   192.168.1.10   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0\n" +
				"node-2   Ready    <none>          5d    v1.28.0   192.168.1.11   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0",
			ip:       mustParseIP("192.168.1.11"),
			wantNode: "node-2",
			wantErr:  false,
		},
		{
			name:        "IP not found",
			mockOutput:  "node-1   Ready    control-plane   5d    v1.28.0   192.168.1.10   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0",
			ip:          mustParseIP("192.168.1.99"),
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:        "kubectl command fails",
			mockOutput:  "",
			mockErr:     errors.New("kubectl: command not found"),
			ip:          mustParseIP("192.168.1.10"),
			wantErr:     true,
			errContains: "kubectl get nodes",
		},
		{
			name:        "empty output",
			mockOutput:  "",
			ip:          mustParseIP("192.168.1.10"),
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:        "malformed output - too few fields",
			mockOutput:  "node-1   Ready",
			ip:          mustParseIP("192.168.1.10"),
			wantErr:     true,
			errContains: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				assert.Equal(t, "kubectl", name)
				assert.Contains(t, args, "get")
				assert.Contains(t, args, "nodes")

				if tt.mockErr != nil {
					if runtime.GOOS == "windows" {
						return exec.Command("cmd", "/c", "exit", "1")
					}
					return exec.Command("false")
				}

				output := tt.mockOutput
				if runtime.GOOS == "windows" {
					output = strings.ReplaceAll(output, "\n", "\r\n")
				}

				return mockExecCmd(output, 0)
			}

			client, _ := newTestClient(t)
			ctx := context.Background()

			got, err := client.GetNodeNameByIP(ctx, tt.ip)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantNode, got)
			}
		})
	}
}

func TestClient_DrainNode(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	tests := []struct {
		name        string
		nodeName    string
		mockErr     error
		wantErr     bool
		errContains string
	}{
		{
			name:     "successful drain",
			nodeName: "test-node",
			wantErr:  false,
		},
		{
			name:        "cordon fails",
			nodeName:    "test-node",
			mockErr:     errors.New("connection refused"),
			wantErr:     true,
			errContains: "kubectl cordon",
		},
		{
			name:     "empty node name",
			nodeName: "",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				callCount++
				assert.Equal(t, "kubectl", name)

				if callCount == 1 {
					assert.Contains(t, args, "cordon")
				} else if callCount == 2 {
					assert.Contains(t, args, "drain")
					assert.Contains(t, args, "--ignore-daemonsets")
					assert.Contains(t, args, "--delete-emptydir-data")
				}

				if tt.mockErr != nil {
					if runtime.GOOS == "windows" {
						return exec.Command("cmd", "/c", "exit", "1")
					}
					return exec.Command("false")
				}
				return mockExecCmd("success", 0)
			}

			client, _ := newTestClient(t)
			ctx := context.Background()

			err := client.DrainNode(ctx, tt.nodeName)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestClient_DeleteNode(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	tests := []struct {
		name        string
		nodeName    string
		mockErr     error
		wantErr     bool
		errContains string
	}{
		{
			name:     "successful delete",
			nodeName: "test-node",
			wantErr:  false,
		},
		{
			name:        "delete fails",
			nodeName:    "test-node",
			mockErr:     errors.New("node not found"),
			wantErr:     true,
			errContains: "kubectl delete node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				assert.Equal(t, "kubectl", name)
				assert.Contains(t, args, "delete")
				assert.Contains(t, args, "node")
				assert.Contains(t, args, tt.nodeName)

				if tt.mockErr != nil {
					if runtime.GOOS == "windows" {
						return exec.Command("cmd", "/c", "exit", "1")
					}
					return exec.Command("false")
				}
				return mockExecCmd("node deleted", 0)
			}

			client, _ := newTestClient(t)
			ctx := context.Background()

			err := client.DeleteNode(ctx, tt.nodeName)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestClient_ClusterInfo(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	t.Run("successful cluster info", func(t *testing.T) {
		expectedOutput := "Kubernetes control plane is running at https://192.168.1.10:6443"

		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			assert.Equal(t, "kubectl", name)
			assert.Contains(t, args, "cluster-info")
			return mockExecCmd(expectedOutput, 0)
		}

		client, _ := newTestClient(t)
		ctx := context.Background()

		got, err := client.ClusterInfo(ctx)
		assert.NoError(t, err)
		assert.Contains(t, strings.TrimSpace(got), "Kubernetes control plane")
	})

	t.Run("cluster info fails", func(t *testing.T) {
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if runtime.GOOS == "windows" {
				return exec.Command("cmd", "/c", "exit", "1")
			}
			return exec.Command("false")
		}

		client, _ := newTestClient(t)
		ctx := context.Background()

		_, err := client.ClusterInfo(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "kubectl cluster-info")
	})
}

func TestClient_GetNodes(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	t.Run("successful get nodes", func(t *testing.T) {
		expectedOutput := "NAME     STATUS   ROLES           AGE   VERSION\nnode-1   Ready    control-plane   5d    v1.28.0"

		if runtime.GOOS == "windows" {
			expectedOutput = strings.ReplaceAll(expectedOutput, "\n", "\r\n")
		}

		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			assert.Equal(t, "kubectl", name)
			assert.Contains(t, args, "get")
			assert.Contains(t, args, "nodes")
			assert.Contains(t, args, "-o")
			assert.Contains(t, args, "wide")
			return mockExecCmd(expectedOutput, 0)
		}

		client, _ := newTestClient(t)
		ctx := context.Background()

		got, err := client.GetNodes(ctx)
		assert.NoError(t, err)
		got = strings.ReplaceAll(got, "\r\n", "\n")
		assert.Contains(t, got, "node-1")
	})

	t.Run("get nodes fails", func(t *testing.T) {
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if runtime.GOOS == "windows" {
				return exec.Command("cmd", "/c", "exit", "1")
			}
			return exec.Command("false")
		}

		client, _ := newTestClient(t)
		ctx := context.Background()

		_, err := client.GetNodes(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "kubectl get nodes")
	})
}

func TestClient_ContextCancellation(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	t.Run("GetNodeNameByIP context cancelled", func(t *testing.T) {
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			select {
			case <-ctx.Done():
				if runtime.GOOS == "windows" {
					return exec.Command("cmd", "/c", "exit", "1")
				}
				return exec.Command("false")
			case <-time.After(100 * time.Millisecond):
				return mockExecCmd("output", 0)
			}
		}

		client, _ := newTestClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := client.GetNodeNameByIP(ctx, mustParseIP("192.168.1.1"))
		assert.Error(t, err)
	})

	t.Run("DrainNode context cancelled", func(t *testing.T) {
		callCount := 0
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			callCount++
			select {
			case <-ctx.Done():
				if runtime.GOOS == "windows" {
					return exec.Command("cmd", "/c", "exit", "1")
				}
				return exec.Command("false")
			default:
				if callCount == 1 {
					return mockExecCmd("cordoned", 0)
				}
				return mockExecCmd("drained", 0)
			}
		}

		client, _ := newTestClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := client.DrainNode(ctx, "test-node")
		assert.Error(t, err)
	})
}

func TestClient_Timeout(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	t.Run("DrainNode timeout during drain phase", func(t *testing.T) {
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if contains(args, "cordon") {
				return mockExecCmd("cordoned", 0)
			}
			select {
			case <-ctx.Done():
				if runtime.GOOS == "windows" {
					return exec.Command("cmd", "/c", "exit", "1")
				}
				return exec.Command("false")
			case <-time.After(100 * time.Millisecond):
				return mockExecCmd("drained", 0)
			}
		}

		client, _ := newTestClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := client.DrainNode(ctx, "test-node")
		assert.Error(t, err)
	})
}

func TestClient_ErrorWrapping(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	t.Run("errors are wrapped correctly", func(t *testing.T) {
		execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if runtime.GOOS == "windows" {
				return exec.Command("cmd", "/c", "exit", "1")
			}
			return exec.Command("false")
		}

		client, _ := newTestClient(t)
		ctx := context.Background()

		_, err := client.GetNodeNameByIP(ctx, mustParseIP("192.168.1.1"))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "kubectl get nodes")
	})
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func TestIntegration_Client(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not found in PATH, skipping integration tests")
	}

	client, _ := newTestClient(t)
	ctx := context.Background()

	t.Run("ClusterInfo", func(t *testing.T) {
		info, err := client.ClusterInfo(ctx)
		if err != nil {
			t.Logf("ClusterInfo error: %v", err)
		} else {
			t.Logf("ClusterInfo: %s", info)
		}
	})

	t.Run("GetNodes", func(t *testing.T) {
		nodes, err := client.GetNodes(ctx)
		if err != nil {
			t.Logf("GetNodes error: %v", err)
		} else {
			t.Logf("Nodes: %s", nodes)
		}
	})
}

func BenchmarkGetNodeNameByIP_Parsing(b *testing.B) {
	output := "node-1   Ready    control-plane   5d    v1.28.0   192.168.1.10   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0\n" +
		"node-2   Ready    <none>          5d    v1.28.0   192.168.1.11   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0\n" +
		"node-3   Ready    <none>          5d    v1.28.0   192.168.1.12   <none>   Talos (v1.5.0)   5.15.0   containerd://1.7.0"
	targetIP := "192.168.1.11"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 6 && fields[5] == targetIP {
				_ = fields[0]
				break
			}
		}
	}
}

func captureOutput(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
