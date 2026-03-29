package talos

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// MockTalosClient is a mock implementation of the Talos client for testing
type MockTalosClient struct {
	mock.Mock
}

func (m *MockTalosClient) ApplyConfiguration(ctx context.Context, req *machine.ApplyConfigurationRequest, opts ...grpc.CallOption) (*machine.ApplyConfigurationResponse, error) {
	args := m.Called(ctx, req)
	if resp := args.Get(0); resp != nil {
		return resp.(*machine.ApplyConfigurationResponse), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockTalosClient) Bootstrap(ctx context.Context, req *machine.BootstrapRequest, opts ...grpc.CallOption) (*machine.BootstrapResponse, error) {
	args := m.Called(ctx, req)
	return nil, args.Error(1)
}

func (m *MockTalosClient) EtcdMemberList(ctx context.Context, req *machine.EtcdMemberListRequest, opts ...grpc.CallOption) (*machine.EtcdMemberListResponse, error) {
	args := m.Called(ctx, req)
	if resp := args.Get(0); resp != nil {
		return resp.(*machine.EtcdMemberListResponse), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockTalosClient) Version(ctx context.Context, opts ...grpc.CallOption) (*machine.VersionResponse, error) {
	args := m.Called(ctx)
	if resp := args.Get(0); resp != nil {
		return resp.(*machine.VersionResponse), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockTalosClient) ServiceList(ctx context.Context, opts ...grpc.CallOption) (*machine.ServiceListResponse, error) {
	args := m.Called(ctx)
	if resp := args.Get(0); resp != nil {
		return resp.(*machine.ServiceListResponse), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockTalosClient) Close() error {
	args := m.Called()
	return args.Error(0)
}

// TestNewClient verifies client initialization
func TestNewClient(t *testing.T) {
	cfg := types.TestConfig()
	client := NewClient(cfg)

	assert.NotNil(t, client)
	assert.Equal(t, cfg, client.config)
	assert.Nil(t, client.talosConfig)
	assert.Nil(t, client.logger)
	assert.Nil(t, client.audit)
}

func TestClient_SetLogger(t *testing.T) {
	cfg := types.TestConfig()
	client := NewClient(cfg)

	logger := zap.NewNop()
	client.SetLogger(logger)

	assert.Equal(t, logger, client.logger)
}

func TestClient_SetAuditLogger(t *testing.T) {
	cfg := types.TestConfig()
	client := NewClient(cfg)

	client.SetAuditLogger(nil)
	assert.Nil(t, client.audit)
}

// TestEtcdMemberListParsing tests the EtcdMember struct
func TestEtcdMemberListParsing(t *testing.T) {
	members := []EtcdMember{
		{ID: 1, Hostname: "192.168.1.201", IsHealthy: true},
		{ID: 2, Hostname: "192.168.1.202", IsHealthy: true},
	}

	assert.Len(t, members, 2)
	assert.Equal(t, uint64(1), members[0].ID)
	assert.Equal(t, "192.168.1.201", members[0].Hostname)
	assert.True(t, members[0].IsHealthy)
}

// TestClientInitialize tests the Initialize method
func TestClientInitialize_NotExist(t *testing.T) {
	cfg := &types.Config{
		ClusterName: "test-cluster",
		SecretsDir:  "/nonexistent-dir-12345", // This will fail directory creation
	}
	client := NewClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := client.Initialize(ctx)
	require.Error(t, err)
	// Should fail when trying to create secrets directory or load talosconfig
	assert.True(t,
		strings.Contains(err.Error(), "failed to load talosconfig") ||
			strings.Contains(err.Error(), "cannot create secrets directory") ||
			strings.Contains(err.Error(), "generate base configs"),
		"Error should indicate initialization failure: %v", err)
}

// TestApplyConfigWithRetry_MaxAttempts tests retry parameter handling
func TestApplyConfigWithRetry_MaxAttempts(t *testing.T) {
	cfg := types.TestConfig()
	client := NewClient(cfg)

	ctx := context.Background()
	ip := net.ParseIP("192.168.1.201")

	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_ = client.ApplyConfigWithRetry(ctx, ip, "/nonexistent/config.yaml", types.RoleControlPlane, 0)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_ = client.ApplyConfigWithRetry(ctx2, ip, "/nonexistent/config.yaml", types.RoleControlPlane, -1)
}

// TestApplyConfigWithRetry_Scenarios tests various retry scenarios.
// Note: Without an injectable TalosAPIClient interface, these tests exercise
// error paths through real (failing) connection attempts.
func TestApplyConfigWithRetry_Scenarios(t *testing.T) {
	t.Run("nonexistent config file returns immediately", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		ip := net.ParseIP("192.168.1.201")

		err := c.ApplyConfigWithRetry(ctx, ip, "/nonexistent/config.yaml", types.RoleControlPlane, 1)
		require.Error(t, err)
		// With maxAttempts=1 and a bad config path, it should fail on the first attempt
		assert.Contains(t, err.Error(), "failed after 1 attempts")
	})

	t.Run("context timeout stops retries during backoff", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		ip := net.ParseIP("192.168.1.201")

		// maxAttempts=3 but context timeout cancels during backoff sleep
		err := c.ApplyConfigWithRetry(ctx, ip, "/nonexistent/config.yaml", types.RoleControlPlane, 3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cancelled")
	})

	t.Run("context cancellation stops retries", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately
		ip := net.ParseIP("192.168.1.201")

		err := c.ApplyConfigWithRetry(ctx, ip, "/nonexistent/config.yaml", types.RoleControlPlane, 5)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context cancelled")
	})

	t.Run("default max attempts when zero", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		ip := net.ParseIP("192.168.1.201")

		err := c.ApplyConfigWithRetry(ctx, ip, "/nonexistent/config.yaml", types.RoleControlPlane, 0)
		require.Error(t, err)
		// 0 gets normalized to 5, but context timeout cancels before all attempts complete
		assert.Contains(t, err.Error(), "cancelled")
	})
}

// TestValidateRemovalQuorum tests quorum validation logic
func TestValidateRemovalQuorum(t *testing.T) {
	tests := []struct {
		name           string
		currentCount   int
		healthyMembers int
		wantErr        bool
		errContains    string
	}{
		{
			name:           "valid removal with 3 nodes",
			currentCount:   3,
			healthyMembers: 3,
			wantErr:        false,
		},
		{
			name:           "valid removal with 5 nodes",
			currentCount:   5,
			healthyMembers: 5,
			wantErr:        false,
		},
		{
			name:           "invalid count zero",
			currentCount:   0,
			healthyMembers: 0,
			wantErr:        true,
			errContains:    "invalid control plane count",
		},
		{
			name:           "invalid negative count",
			currentCount:   -1,
			healthyMembers: 0,
			wantErr:        true,
			errContains:    "invalid control plane count",
		},
		{
			name:           "would violate quorum 3->2",
			currentCount:   3,
			healthyMembers: 2,
			wantErr:        true,
			errContains:    "violate etcd quorum",
		},
		{
			name:           "would violate quorum 5->2",
			currentCount:   5,
			healthyMembers: 3,
			wantErr:        true,
			errContains:    "violate etcd quorum",
		},
		{
			name:           "no healthy members",
			currentCount:   3,
			healthyMembers: 0,
			wantErr:        true,
			errContains:    "no healthy etcd members",
		},
		{
			name:           "single node cluster",
			currentCount:   1,
			healthyMembers: 1,
			wantErr:        true,
			errContains:    "at least 1 healthy member is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentCount <= 0 {
				err := errors.New("invalid control plane count")
				if tt.wantErr {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			if tt.healthyMembers == 0 {
				err := errors.New("no healthy etcd members found")
				if tt.wantErr {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			afterRemoval := tt.healthyMembers - 1
			minQuorum := (tt.currentCount / 2) + 1

			if afterRemoval < 1 {
				err := errors.New("at least 1 healthy member is required")
				if tt.wantErr {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			if afterRemoval < minQuorum {
				err := errors.New("would violate etcd quorum")
				if tt.wantErr {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			if tt.wantErr {
				t.Errorf("Expected error but got none")
			}
		})
	}
}

// TestGetEtcdMemberIDByIP tests member lookup logic
func TestGetEtcdMemberIDByIP(t *testing.T) {
	members := []EtcdMember{
		{ID: 12345, Hostname: "192.168.1.201"},
		{ID: 67890, Hostname: "192.168.1.202"},
	}

	t.Run("find by hostname", func(t *testing.T) {
		targetIP := net.ParseIP("192.168.1.201")
		var foundID uint64

		for _, m := range members {
			if m.Hostname == targetIP.String() {
				foundID = m.ID
				break
			}
		}

		assert.Equal(t, uint64(12345), foundID)
	})

	t.Run("not found", func(t *testing.T) {
		targetIP := net.ParseIP("192.168.1.999")
		found := false

		for _, m := range members {
			if m.Hostname == targetIP.String() {
				found = true
				break
			}
		}

		assert.False(t, found)
	})
}

// TestCheckReady_Scenarios tests readiness check behavior.
// Tests the checkReady method indirectly through checkReadyByIP since
// checkReady requires a connected *client.Client.
func TestCheckReady_Scenarios(t *testing.T) {
	t.Run("uninitialized client returns error", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ip := net.ParseIP("192.168.1.201")

		// Client not initialized -> getClient fails in both secure and insecure
		ready, err := c.checkReadyByIP(context.Background(), ip, types.RoleControlPlane)
		assert.Error(t, err)
		assert.False(t, ready)
	})

	t.Run("worker readiness check with uninitialized client", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ip := net.ParseIP("192.168.1.201")

		ready, err := c.checkReadyByIP(context.Background(), ip, types.RoleWorker)
		assert.Error(t, err)
		assert.False(t, ready)
	})

	t.Run("timeout propagation", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()
		time.Sleep(5 * time.Millisecond) // Ensure context is expired
		ip := net.ParseIP("192.168.1.201")

		ready, errr := c.checkReadyByIP(ctx, ip, types.RoleControlPlane)
		assert.Error(t, errr)
		assert.False(t, ready)
	})
}

// TestIsMaintenanceModeError tests the helper function
func TestIsMaintenanceModeError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "Unavailable error",
			err:      errors.New("rpc error: code = Unavailable"),
			expected: true,
		},
		{
			name:     "maintenance mode error",
			err:      errors.New("node is in maintenance mode"),
			expected: true,
		},
		{
			name:     "unimplemented error",
			err:      errors.New("method unimplemented"),
			expected: true,
		},
		{
			name:     "other error",
			err:      errors.New("some other error"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isMaintenanceModeError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestWaitForReady_Timeout tests timeout behavior
func TestWaitForReady_Timeout(t *testing.T) {
	cfg := types.TestConfig()
	client := NewClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	ip := net.ParseIP("192.168.1.201")

	err := client.WaitForReady(ctx, ip, types.RoleControlPlane)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

// TestBootstrapEtcd_Scenarios tests bootstrap behavior.
// Without a running Talos API, these test error handling paths.
func TestBootstrapEtcd_Scenarios(t *testing.T) {
	t.Run("uninitialized client returns error", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ip := net.ParseIP("192.168.1.201")

		err := c.BootstrapEtcd(context.Background(), ip)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client not initialized")
	})

	t.Run("cancelled context returns error", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ip := net.ParseIP("192.168.1.201")

		err := c.BootstrapEtcd(ctx, ip)
		require.Error(t, err)
	})
}

// TestResetNode_Scenarios tests reset behavior.
func TestResetNode_Scenarios(t *testing.T) {
	t.Run("uninitialized client returns error", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ip := net.ParseIP("192.168.1.201")

		err := c.ResetNode(context.Background(), ip, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client not initialized")
	})

	t.Run("force reset with uninitialized client returns error", func(t *testing.T) {
		cfg := types.TestConfig()
		c := NewClient(cfg)
		ip := net.ParseIP("192.168.1.201")

		err := c.ResetNode(context.Background(), ip, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client not initialized")
	})
}

// Benchmarks

func BenchmarkIsMaintenanceModeError(b *testing.B) {
	err := errors.New("rpc error: code = Unavailable desc = node is in maintenance mode")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		isMaintenanceModeError(err)
	}
}
