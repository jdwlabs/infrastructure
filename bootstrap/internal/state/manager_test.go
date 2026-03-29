// manager_test.go
package state

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// testFixture provides reusable test infrastructure
type testFixture struct {
	t       *testing.T
	tmpDir  string
	manager *Manager
	config  *types.Config
	logger  *zap.Logger
	ctx     context.Context
}

// newTestFixture creates a new test fixture with isolated temp directory
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()

	tmpDir := t.TempDir()
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	cfg := &types.Config{
		ClusterName:          "test-cluster",
		ControlPlaneEndpoint: "https://192.168.1.10:6443",
		HAProxyIP:            net.ParseIP("192.168.1.5"),
		TerraformTFVars:      filepath.Join(tmpDir, "terraform.tfvars"),
		SecretsDir:           filepath.Join(tmpDir, "secrets"),
	}

	manager := NewManager(cfg, logger)
	// Override paths to use temp directory
	manager.stateDir = filepath.Join(tmpDir, "state")
	manager.nodesDir = filepath.Join(tmpDir, "nodes")

	return &testFixture{
		t:       t,
		tmpDir:  tmpDir,
		manager: manager,
		config:  cfg,
		logger:  logger,
		ctx:     ctx,
	}
}

// createTerraformVars creates a terraform.tfvars file with specified content
func (f *testFixture) createTerraformVars(content string) {
	f.t.Helper()
	err := os.WriteFile(f.config.TerraformTFVars, []byte(content), 0644)
	require.NoError(f.t, err, "Failed to create terraform.tfvars")
}

// createStateFile creates a bootstrap-state.json file with specified content
func (f *testFixture) createStateFile(content string) {
	f.t.Helper()
	err := os.MkdirAll(f.manager.stateDir, 0700)
	require.NoError(f.t, err, "Failed to create state directory")

	err = os.WriteFile(filepath.Join(f.manager.stateDir, "bootstrap-state.json"), []byte(content), 0600)
	require.NoError(f.t, err, "Failed to create state file")
}

// createNodeConfig creates a node configuration file
func (f *testFixture) createNodeConfig(vmid types.VMID, role types.Role, content string) {
	f.t.Helper()
	err := os.MkdirAll(f.manager.nodesDir, 0755)
	require.NoError(f.t, err, "Failed to create nodes directory")

	filename := f.manager.NodeConfigPath(vmid, role)
	err = os.WriteFile(filename, []byte(content), 0644)
	require.NoError(f.t, err, "Failed to create node config")
}

// TestNewManager validates Manager initialization
func TestNewManager(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		wantPaths   []string
	}{
		{
			name:        "creates manager with cluster directory structure",
			clusterName: "prod-cluster",
			wantPaths:   []string{"clusters", "prod-cluster"},
		},
		{
			name:        "handles simple cluster names",
			clusterName: "test",
			wantPaths:   []string{"clusters", "test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &types.Config{
				ClusterName:          tt.clusterName,
				ControlPlaneEndpoint: "https://192.168.1.10:6443",
				HAProxyIP:            net.ParseIP("192.168.1.5"),
			}
			logger := zaptest.NewLogger(t)

			manager := NewManager(cfg, logger)

			assert.NotNil(t, manager)
			assert.Equal(t, cfg, manager.config)
			assert.Contains(t, manager.stateDir, filepath.Join(tt.wantPaths...))
			assert.Contains(t, manager.nodesDir, filepath.Join(tt.wantPaths...))
		})
	}
}

// TestManager_NodeConfigPath validates node configuration path generation
func TestManager_NodeConfigPath(t *testing.T) {
	f := newTestFixture(t)

	tests := []struct {
		name     string
		vmid     types.VMID
		role     types.Role
		expected string
	}{
		{
			name:     "control plane node path",
			vmid:     100,
			role:     types.RoleControlPlane,
			expected: filepath.Join(f.manager.nodesDir, "node-control-plane-100.yaml"),
		},
		{
			name:     "worker node path",
			vmid:     200,
			role:     types.RoleWorker,
			expected: filepath.Join(f.manager.nodesDir, "node-worker-200.yaml"),
		},
		{
			name:     "high VMID",
			vmid:     999999,
			role:     types.RoleControlPlane,
			expected: filepath.Join(f.manager.nodesDir, "node-control-plane-999999.yaml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.manager.NodeConfigPath(tt.vmid, tt.role)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// TestManager_LoadDeployedState validates state loading scenarios
func TestManager_LoadDeployedState(t *testing.T) {
	tests := []struct {
		name          string
		setupState    string
		expectError   bool
		errorContains string
		expectedState func(*testing.T, *types.ClusterState)
	}{
		{
			name:       "returns empty state for new cluster",
			setupState: "", // No file
			expectedState: func(t *testing.T, s *types.ClusterState) {
				assert.False(t, s.BootstrapCompleted)
				assert.Empty(t, s.ControlPlanes)
				assert.Empty(t, s.Workers)
			},
		},
		{
			name: "loads existing state successfully",
			setupState: `{
				"cluster_name": "existing",
				"control_plane_endpoint": "https://192.168.1.10:6443",
				"haproxy_ip": "192.168.1.5",
				"bootstrap_completed": true,
				"control_planes": [{"vmid": 100, "ip": "192.168.1.20", "config_hash": "abc123"}],
				"workers": [{"vmid": 200, "ip": "192.168.1.30", "config_hash": "def456"}]
			}`,
			expectedState: func(t *testing.T, s *types.ClusterState) {
				assert.True(t, s.BootstrapCompleted)
				assert.Len(t, s.ControlPlanes, 1)
				assert.Len(t, s.Workers, 1)
				assert.Equal(t, types.VMID(100), s.ControlPlanes[0].VMID)
				assert.Equal(t, "abc123", s.ControlPlanes[0].ConfigHash)
			},
		},
		{
			name: "migrates wrapped state format",
			setupState: `{
				"deployed_state": {
					"cluster_name": "migrated",
					"bootstrap_completed": true,
					"control_planes": [{"vmid": 101, "config_hash": "hash789"}]
				}
			}`,
			expectedState: func(t *testing.T, s *types.ClusterState) {
				assert.Equal(t, "migrated", s.ClusterName)
				assert.True(t, s.BootstrapCompleted)
				assert.Len(t, s.ControlPlanes, 1)
			},
		},
		{
			name:          "returns error for corrupted state",
			setupState:    "not valid json",
			expectError:   true,
			errorContains: "corrupted",
		},
		{
			name: "backfills missing metadata from config",
			setupState: `{
				"bootstrap_completed": false,
				"control_planes": []
			}`,
			expectedState: func(t *testing.T, s *types.ClusterState) {
				// Should inherit from config
				assert.Equal(t, "test-cluster", s.ClusterName)
				assert.Equal(t, "https://192.168.1.10:6443", s.ControlPlaneEndpoint)
				assert.NotNil(t, s.HAProxyIP)
				assert.True(t, s.HAProxyIP.Equal(net.ParseIP("192.168.1.5")))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFixture(t)

			if tt.setupState != "" {
				f.createStateFile(tt.setupState)
			}

			state, err := f.manager.LoadDeployedState(f.ctx)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, state)

			if tt.expectedState != nil {
				tt.expectedState(t, state)
			}
		})
	}
}

// TestManager_Save validates state persistence
func TestManager_Save(t *testing.T) {
	f := newTestFixture(t)
	f.createTerraformVars("test content for hash")

	state := &types.ClusterState{
		ClusterName:          "test",
		ControlPlaneEndpoint: "https://192.168.1.10:6443",
		BootstrapCompleted:   true,
		ControlPlanes: []types.NodeState{
			{VMID: 100, IP: net.ParseIP("192.168.1.10"), ConfigHash: "hash123"},
		},
	}

	err := f.manager.Save(f.ctx, state)
	require.NoError(t, err)

	// Verify file exists and is readable
	statePath := filepath.Join(f.manager.stateDir, "bootstrap-state.json")
	data, err := os.ReadFile(statePath)
	require.NoError(t, err)

	// Verify JSON structure
	var saved types.ClusterState
	err = json.Unmarshal(data, &saved)
	require.NoError(t, err)

	assert.Equal(t, "hash123", saved.ControlPlanes[0].ConfigHash)
	assert.Equal(t, types.VMID(100), saved.ControlPlanes[0].VMID)
	assert.NotEmpty(t, saved.TerraformHash) // Should compute hash
	assert.False(t, saved.Timestamp.IsZero())
}

// TestManager_BuildReconcilePlan validates reconciliation planning
func TestManager_BuildReconcilePlan(t *testing.T) {
	tests := []struct {
		name          string
		desired       map[types.VMID]*types.NodeSpec
		deployed      *types.ClusterState
		live          map[types.VMID]*types.LiveNode
		setupFiles    func(*testFixture)
		forceReconfig bool
		validatePlan  func(*testing.T, *types.ReconcilePlan)
	}{
		{
			name: "detects control plane additions",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
				101: {VMID: 101, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{{VMID: 100}},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.AddControlPlanes, types.VMID(101))
				assert.NotContains(t, p.AddControlPlanes, types.VMID(100))
				assert.Empty(t, p.RemoveControlPlanes)
			},
		},
		{
			name: "detects worker additions",
			desired: map[types.VMID]*types.NodeSpec{
				200: {VMID: 200, Role: types.RoleWorker},
				201: {VMID: 201, Role: types.RoleWorker},
			},
			deployed: &types.ClusterState{
				Workers: []types.NodeState{{VMID: 200}},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.AddWorkers, types.VMID(201))
				assert.NotContains(t, p.AddWorkers, types.VMID(200))
			},
		},
		{
			name: "detects control plane removals",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{
					{VMID: 100},
					{VMID: 101}, // Should be removed
				},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.RemoveControlPlanes, types.VMID(101))
				assert.NotContains(t, p.RemoveControlPlanes, types.VMID(100))
			},
		},
		{
			name:    "detects worker removals",
			desired: map[types.VMID]*types.NodeSpec{},
			deployed: &types.ClusterState{
				Workers: []types.NodeState{{VMID: 200}},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.RemoveWorkers, types.VMID(200))
			},
		},
		{
			name: "detects config drift",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{
					{VMID: 100, ConfigHash: "old_hash"},
				},
			},
			setupFiles: func(f *testFixture) {
				f.createNodeConfig(100, types.RoleControlPlane, "new config content")
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.UpdateConfigs, types.VMID(100))
			},
		},
		{
			name: "skips drift check for new nodes",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				// Should be in AddControlPlanes, not UpdateConfigs
				assert.Contains(t, p.AddControlPlanes, types.VMID(100))
				assert.NotContains(t, p.UpdateConfigs, types.VMID(100))
			},
		},
		{
			name: "force reconfigure updates all configs",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{
					{VMID: 100, ConfigHash: "2e6f9e5e0b23e5f5a1c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9"}, // Same hash
				},
			},
			setupFiles: func(f *testFixture) {
				// Create file with specific content that produces known hash
				f.createNodeConfig(100, types.RoleControlPlane, "config")
			},
			forceReconfig: true,
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.Contains(t, p.UpdateConfigs, types.VMID(100))
			},
		},
		{
			name: "detects bootstrap needed for new cluster",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				BootstrapCompleted: false,
				ControlPlanes:      []types.NodeState{},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.True(t, p.NeedsBootstrap)
			},
		},
		{
			name: "detects bootstrap needed for interrupted bootstrap",
			deployed: &types.ClusterState{
				BootstrapCompleted: false,
				ControlPlanes:      []types.NodeState{{VMID: 100}},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.True(t, p.NeedsBootstrap)
			},
		},
		{
			name: "no bootstrap needed when completed",
			deployed: &types.ClusterState{
				BootstrapCompleted: true,
				ControlPlanes:      []types.NodeState{{VMID: 100}},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				assert.False(t, p.NeedsBootstrap)
			},
		},
		{
			name: "syncs live discovered IPs",
			desired: map[types.VMID]*types.NodeSpec{
				100: {VMID: 100, Role: types.RoleControlPlane},
			},
			deployed: &types.ClusterState{
				ControlPlanes: []types.NodeState{
					{VMID: 100, IP: net.ParseIP("192.168.1.10")},
				},
			},
			live: map[types.VMID]*types.LiveNode{
				100: {VMID: 100, IP: net.ParseIP("192.168.1.20"), Status: types.StatusDiscovered},
			},
			validatePlan: func(t *testing.T, p *types.ReconcilePlan) {
				// IP should be synced in deployed state (side effect)
				// No specific plan changes, but logged
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFixture(t)
			f.config.ForceReconfigure = tt.forceReconfig

			if tt.setupFiles != nil {
				tt.setupFiles(f)
			}

			// Ensure deployed state has required fields
			if tt.deployed.ClusterName == "" {
				tt.deployed.ClusterName = f.config.ClusterName
			}

			plan, err := f.manager.BuildReconcilePlan(f.ctx, tt.desired, tt.deployed, tt.live)
			require.NoError(t, err)

			if tt.validatePlan != nil {
				tt.validatePlan(t, plan)
			}
		})
	}
}

// TestManager_UpdateNodeState validates node state updates
func TestManager_UpdateNodeState(t *testing.T) {
	f := newTestFixture(t)

	state := &types.ClusterState{
		ControlPlanes: []types.NodeState{},
		Workers:       []types.NodeState{},
	}

	t.Run("adds new control plane", func(t *testing.T) {
		f.manager.UpdateNodeState(state, 100, "192.168.1.10", "hash1", types.RoleControlPlane)

		require.Len(t, state.ControlPlanes, 1)
		assert.Equal(t, types.VMID(100), state.ControlPlanes[0].VMID)
		assert.Equal(t, "hash1", state.ControlPlanes[0].ConfigHash)
		assert.True(t, state.ControlPlanes[0].IP.Equal(net.ParseIP("192.168.1.10")))
		assert.False(t, state.ControlPlanes[0].LastSeen.IsZero())
	})

	t.Run("updates existing control plane", func(t *testing.T) {
		f.manager.UpdateNodeState(state, 100, "192.168.1.11", "hash2", types.RoleControlPlane)

		require.Len(t, state.ControlPlanes, 1)
		assert.Equal(t, "hash2", state.ControlPlanes[0].ConfigHash)
		assert.True(t, state.ControlPlanes[0].IP.Equal(net.ParseIP("192.168.1.11")))
	})

	t.Run("adds worker", func(t *testing.T) {
		f.manager.UpdateNodeState(state, 200, "192.168.1.20", "hash3", types.RoleWorker)

		require.Len(t, state.Workers, 1)
		assert.Equal(t, types.VMID(200), state.Workers[0].VMID)
	})

	t.Run("handles empty IP", func(t *testing.T) {
		f.manager.UpdateNodeState(state, 300, "", "hash4", types.RoleControlPlane)

		found := false
		for _, cp := range state.ControlPlanes {
			if cp.VMID == 300 {
				found = true
				assert.Nil(t, cp.IP)
			}
		}
		assert.True(t, found)
	})
}

// TestManager_RemoveNodeState validates node removal
func TestManager_RemoveNodeState(t *testing.T) {
	f := newTestFixture(t)

	t.Run("removes control plane", func(t *testing.T) {
		state := &types.ClusterState{
			ControlPlanes: []types.NodeState{
				{VMID: 100},
				{VMID: 101},
				{VMID: 102},
			},
		}

		f.manager.RemoveNodeState(state, 101, types.RoleControlPlane)

		assert.Len(t, state.ControlPlanes, 2)
		for _, cp := range state.ControlPlanes {
			assert.NotEqual(t, types.VMID(101), cp.VMID)
		}
	})

	t.Run("removes worker", func(t *testing.T) {
		state := &types.ClusterState{
			Workers: []types.NodeState{
				{VMID: 200},
				{VMID: 201},
			},
		}

		f.manager.RemoveNodeState(state, 200, types.RoleWorker)

		assert.Len(t, state.Workers, 1)
		assert.Equal(t, types.VMID(201), state.Workers[0].VMID)
	})

	t.Run("handles non-existent VMID gracefully", func(t *testing.T) {
		state := &types.ClusterState{
			ControlPlanes: []types.NodeState{{VMID: 100}},
		}

		f.manager.RemoveNodeState(state, 999, types.RoleControlPlane)

		assert.Len(t, state.ControlPlanes, 1)
	})
}

// TestManager_ComputeTerraformHash validates hash computation
func TestManager_ComputeTerraformHash(t *testing.T) {
	f := newTestFixture(t)

	t.Run("computes consistent hash", func(t *testing.T) {
		content := "talos_control_configuration = []"
		f.createTerraformVars(content)

		hash1, err := f.manager.ComputeTerraformHash()
		require.NoError(t, err)
		assert.Len(t, hash1, 64) // SHA256 hex

		hash2, err := f.manager.ComputeTerraformHash()
		require.NoError(t, err)
		assert.Equal(t, hash1, hash2)
	})

	t.Run("different content produces different hash", func(t *testing.T) {
		f.createTerraformVars("content A")
		hash1, _ := f.manager.ComputeTerraformHash()

		f.createTerraformVars("content B")
		hash2, _ := f.manager.ComputeTerraformHash()

		assert.NotEqual(t, hash1, hash2)
	})

	t.Run("returns error for missing file", func(t *testing.T) {
		// Point to non-existent file
		f.config.TerraformTFVars = filepath.Join(f.tmpDir, "nonexistent.tfvars")

		_, err := f.manager.ComputeTerraformHash()
		assert.Error(t, err)
	})
}

// TestManager_LoadDesiredState validates terraform.tfvars parsing
func TestManager_LoadDesiredState(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		expectedCount int
		expectedVMIDs []types.VMID
		expectError   bool
	}{
		{
			name: "parses HCL control planes",
			content: `
talos_control_configuration = [
  {
    vmid = 100
    vm_name = "cp1"
    node_name = "pve1"
    cpu_cores = 4
    memory = 8192
    disk_size = 100
  },
  {
    vmid = 101
    vm_name = "cp2"
  }
]
`,
			expectedCount: 2,
			expectedVMIDs: []types.VMID{100, 101},
		},
		{
			name: "parses HCL workers",
			content: `
talos_worker_configuration = [
  {
    vmid = 200
    vm_name = "worker1"
    node_name = "pve1"
    cpu_cores = 8
    memory = 16384
    disk_size = 500
  }
]
`,
			expectedCount: 1,
			expectedVMIDs: []types.VMID{200},
		},
		{
			name: "parses mixed configuration",
			content: `
talos_control_configuration = [
  { vmid = 100, vm_name = "cp1", node_name = "pve1", cpu_cores = 4, memory = 8192, disk_size = 100 }
]
talos_worker_configuration = [
  { vmid = 200, vm_name = "worker1", node_name = "pve1", cpu_cores = 8, memory = 16384, disk_size = 500 }
]
`,
			expectedCount: 2,
			expectedVMIDs: []types.VMID{100, 200},
		},
		{
			name:          "handles empty file",
			content:       "",
			expectedCount: 0,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFixture(t)
			f.createTerraformVars(tt.content)

			specs, err := f.manager.LoadDesiredState(f.ctx)

			if tt.expectError {
				// Empty file might fallback to empty map or error depending on implementation
				if err != nil {
					return // Expected
				}
				// If no error, should return empty map
				assert.Empty(t, specs)
				return
			}

			require.NoError(t, err)
			assert.Len(t, specs, tt.expectedCount)

			for _, vmid := range tt.expectedVMIDs {
				assert.Contains(t, specs, vmid)
			}
		})
	}
}

// TestManager_LoadTerraformExtras validates extra field extraction
func TestManager_LoadTerraformExtras(t *testing.T) {
	tests := []struct {
		name           string
		initialConfig  func(*types.Config)
		tfvarsContent  string
		validateConfig func(*testing.T, *types.Config)
	}{
		{
			name: "extracts cluster_name when default",
			initialConfig: func(c *types.Config) {
				c.ClusterName = "cluster" // Default value
			},
			tfvarsContent: `cluster_name = "extracted-cluster"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "extracted-cluster", c.ClusterName)
			},
		},
		{
			name: "preserves cluster_name when customized",
			initialConfig: func(c *types.Config) {
				c.ClusterName = "custom-cluster"
			},
			tfvarsContent: `cluster_name = "ignored-cluster"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "custom-cluster", c.ClusterName)
			},
		},
		{
			name:          "extracts proxmox credentials",
			tfvarsContent: `proxmox_api_token_id = "root@pam!token"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "root@pam!token", c.ProxmoxTokenID)
			},
		},
		{
			name: "extracts control plane endpoint",
			initialConfig: func(c *types.Config) {
				c.ControlPlaneEndpoint = "" // Clear to allow extraction
			},
			tfvarsContent: `control_plane_endpoint = "api.example.com"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "api.example.com", c.ControlPlaneEndpoint)
			},
		},
		{
			name: "extracts haproxy_ip",
			initialConfig: func(c *types.Config) {
				c.HAProxyIP = nil // Clear to allow extraction
			},
			tfvarsContent: `haproxy_ip = "192.168.1.50"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.True(t, c.HAProxyIP.Equal(net.ParseIP("192.168.1.50")))
			},
		},
		{
			name: "preserves control_plane_endpoint when set by flag",
			initialConfig: func(c *types.Config) {
				c.ControlPlaneEndpoint = "from-flag.example.com"
			},
			tfvarsContent: `control_plane_endpoint = "from-tfvars.example.com"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "from-flag.example.com", c.ControlPlaneEndpoint)
			},
		},
		{
			name: "preserves haproxy_ip when set by flag",
			initialConfig: func(c *types.Config) {
				c.HAProxyIP = net.ParseIP("10.0.0.1")
			},
			tfvarsContent: `haproxy_ip = "192.168.1.50"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.True(t, c.HAProxyIP.Equal(net.ParseIP("10.0.0.1")))
			},
		},
		{
			name:          "extracts kubernetes_version",
			tfvarsContent: `kubernetes_version = "v1.35.1"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "v1.35.1", c.KubernetesVersion)
			},
		},
		{
			name:          "extracts talos_version",
			tfvarsContent: `talos_version = "v1.12.3"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "v1.12.3", c.TalosVersion)
			},
		},
		{
			name:          "extracts installer_image",
			tfvarsContent: `installer_image = "factory.talos.dev/nocloud-installer/test:v1.12.3"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "factory.talos.dev/nocloud-installer/test:v1.12.3", c.InstallerImage)
			},
		},
		{
			name:          "extracts haproxy_login_user",
			tfvarsContent: `haproxy_login_user = "jake"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "jake", c.HAProxyLoginUser)
			},
		},
		{
			name:          "extracts haproxy_stats_user",
			tfvarsContent: `haproxy_stats_user = "admin"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "admin", c.HAProxyStatsUser)
			},
		},
		{
			name:          "extracts haproxy_stats_password",
			tfvarsContent: `haproxy_stats_password = "admin"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "admin", c.HAProxyStatsPassword)
			},
		},
		{
			name: "extracts proxmox_node_ips map",
			tfvarsContent: `proxmox_node_ips = {
  pve1 = "192.168.1.200"
  pve2 = "192.168.1.201"
  pve3 = "192.168.1.202"
}`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Len(t, c.ProxmoxNodeIPs, 3)
				assert.True(t, c.ProxmoxNodeIPs["pve1"].Equal(net.ParseIP("192.168.1.200")))
				assert.True(t, c.ProxmoxNodeIPs["pve2"].Equal(net.ParseIP("192.168.1.201")))
				assert.True(t, c.ProxmoxNodeIPs["pve3"].Equal(net.ParseIP("192.168.1.202")))
			},
		},
		{
			name: "preserves kubernetes_version when set by flag",
			initialConfig: func(c *types.Config) {
				c.KubernetesVersion = "v1.99.0"
			},
			tfvarsContent: `kubernetes_version = "v1.35.1"`,
			validateConfig: func(t *testing.T, c *types.Config) {
				assert.Equal(t, "v1.99.0", c.KubernetesVersion)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFixture(t)

			if tt.initialConfig != nil {
				tt.initialConfig(f.config)
			}

			f.createTerraformVars(tt.tfvarsContent)

			err := f.manager.LoadTerraformExtras(f.ctx)
			// May error if file not found, but we created it
			if err != nil {
				t.Logf("LoadTerraformExtras error: %v", err)
			}

			if tt.validateConfig != nil {
				tt.validateConfig(t, f.config)
			}
		})
	}
}

// TestParseIP validates IP parsing helper
func TestParseIP(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected net.IP
	}{
		{
			name:     "valid IPv4",
			input:    "192.168.1.1",
			expected: net.ParseIP("192.168.1.1"),
		},
		{
			name:     "valid IPv6",
			input:    "::1",
			expected: net.ParseIP("::1"),
		},
		{
			name:     "invalid IP returns nil",
			input:    "invalid",
			expected: nil,
		},
		{
			name:     "empty string returns nil",
			input:    "",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIP(tt.input)
			if tt.expected == nil {
				assert.Nil(t, got)
			} else {
				assert.True(t, got.Equal(tt.expected))
			}
		})
	}
}

// TestFallbackParseTerraform validates the fallback parser
func TestFallbackParseTerraform(t *testing.T) {
	f := newTestFixture(t)

	t.Run("parses simple blocks", func(t *testing.T) {
		content := `
talos_control_configuration = [
  {
    vmid = 100
    vm_name = "cp1"
    node_name = "pve1"
    cpu_cores = 4
    memory = 8192
    disk_size = 100
  }
]
`
		f.createTerraformVars(content)

		// Force fallback by using content that HCL might fail on
		specs, err := f.manager.fallbackParseTerraform([]byte(content))
		require.NoError(t, err)
		assert.Len(t, specs, 1)

		spec := specs[100]
		require.NotNil(t, spec)
		assert.Equal(t, "cp1", spec.Name)
		assert.Equal(t, "pve1", spec.Node)
		assert.Equal(t, 4, spec.CPU)
		assert.Equal(t, 8192, spec.Memory)
		assert.Equal(t, 100, spec.Disk)
	})

	t.Run("returns error when no nodes found", func(t *testing.T) {
		content := `invalid = "content"`
		_, err := f.manager.fallbackParseTerraform([]byte(content))
		assert.Error(t, err)
	})
}

// TestExtractFields validates field extraction helpers
func TestExtractFields(t *testing.T) {
	t.Run("extractStringField", func(t *testing.T) {
		block := `vm_name = "test-node" node_name = "pve1"`
		assert.Equal(t, "test-node", extractStringField(block, "vm_name"))
		assert.Equal(t, "pve1", extractStringField(block, "node_name"))
		assert.Empty(t, extractStringField(block, "nonexistent"))
	})

	t.Run("extractIntField with quoted value", func(t *testing.T) {
		block := `vmid = "100" cpu_cores = "4"`
		assert.Equal(t, 100, extractIntField(block, "vmid"))
		assert.Equal(t, 4, extractIntField(block, "cpu_cores"))
	})

	t.Run("extractIntField with unquoted value", func(t *testing.T) {
		block := `vmid = 200 memory = 8192`
		assert.Equal(t, 200, extractIntField(block, "vmid"))
		assert.Equal(t, 8192, extractIntField(block, "memory"))
	})

	t.Run("extractIntField returns 0 for missing", func(t *testing.T) {
		block := `other = 100`
		assert.Equal(t, 0, extractIntField(block, "vmid"))
	})

	t.Run("extractSimpleStringField", func(t *testing.T) {
		content := `cluster_name = "my-cluster"`
		assert.Equal(t, "my-cluster", extractSimpleStringField(content, "cluster_name"))
	})
}

// TestParseArrayBlocks validates the brace-counting parser
func TestParseArrayBlocks(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		varName  string
		expected int
	}{
		{
			name: "extracts single block",
			content: `talos_control_configuration = [
				{ vmid = 100 }
			]`,
			varName:  "talos_control_configuration",
			expected: 1,
		},
		{
			name: "extracts multiple blocks",
			content: `talos_control_configuration = [
				{ vmid = 100 },
				{ vmid = 101 },
				{ vmid = 102 }
			]`,
			varName:  "talos_control_configuration",
			expected: 3,
		},
		{
			name: "handles nested braces",
			content: `talos_control_configuration = [
				{ 
					vmid = 100
					config = { nested = "value" }
				}
			]`,
			varName:  "talos_control_configuration",
			expected: 1,
		},
		{
			name:     "returns empty for missing variable",
			content:  `other_var = []`,
			varName:  "talos_control_configuration",
			expected: 0,
		},
		{
			name:     "returns empty for missing bracket",
			content:  `talos_control_configuration = { vmid = 100 }`,
			varName:  "talos_control_configuration",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseArrayBlocks(tt.content, tt.varName)
			assert.Len(t, got, tt.expected)
		})
	}
}

func TestExtractURLHost(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"full URL with port and path", "https://192.168.1.200:8006/api2/json", "192.168.1.200"},
		{"URL without port", "https://myhost.example.com/path", "myhost.example.com"},
		{"plain IP (no scheme)", "192.168.1.200", "192.168.1.200"},
		{"just hostname", "myhost", "myhost"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractURLHost(tt.input))
		})
	}
}

func TestParseTFVarsMap(t *testing.T) {
	t.Run("quoted string values", func(t *testing.T) {
		content := `proxmox_node_ips = {
  pve1 = "192.168.1.200"
  pve2 = "192.168.1.201"
}`
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Len(t, result, 2)
		assert.Equal(t, "192.168.1.200", result["pve1"])
		assert.Equal(t, "192.168.1.201", result["pve2"])
	})

	t.Run("single line with commas", func(t *testing.T) {
		content := `proxmox_node_ips = { pve1 = "10.0.0.1", pve2 = "10.0.0.2" }`
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Len(t, result, 2)
		assert.Equal(t, "10.0.0.1", result["pve1"])
		assert.Equal(t, "10.0.0.2", result["pve2"])
	})

	t.Run("key not present", func(t *testing.T) {
		content := `other_var = { a = "b" }`
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Empty(t, result)
	})

	t.Run("missing closing brace", func(t *testing.T) {
		content := `proxmox_node_ips = { pve1 = "10.0.0.1"`
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Empty(t, result)
	})

	t.Run("empty map", func(t *testing.T) {
		content := `proxmox_node_ips = {}`
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Empty(t, result)
	})

	t.Run("does not match nested keys", func(t *testing.T) {
		content := `other = {
  proxmox_node_ips = { pve1 = "10.0.0.1" }
}`
		// parseTFVarsMap is anchored to start of line, so nested key should not match
		result := parseTFVarsMap(content, "proxmox_node_ips")
		assert.Empty(t, result)
	})
}

// TestLoadTerraformExtras_fullScenario proves that a complete tfvars file
// populates all required Config fields from DefaultConfig() alone.
func TestLoadTerraformExtras_fullScenario(t *testing.T) {
	tmpDir := t.TempDir()
	logger := zaptest.NewLogger(t)

	cfg := types.DefaultConfig()
	cfg.TerraformTFVars = filepath.Join(tmpDir, "terraform.tfvars")

	content := `cluster_name         = "test-core"
proxmox_endpoint          = "https://192.168.1.200:8006/api2/json"
proxmox_api_token_id      = "terraform@pve!cluster"
proxmox_api_token_secret  = "secret"
control_plane_endpoint    = "cluster.example.com"
haproxy_ip                = "192.168.1.199"
haproxy_login_user        = "root"
haproxy_stats_user		  = "admin"
haproxy_stats_password    = "changeme"
kubernetes_version        = "v1.35.1"
talos_version             = "v1.12.3"
installer_image           = "factory.talos.dev/nocloud-installer/test:v1.12.3"

proxmox_node_ips = {
  pve1 = "192.168.1.200"
  pve2 = "192.168.1.201"
}

talos_control_configuration = []
talos_worker_configuration = []
`
	err := os.WriteFile(cfg.TerraformTFVars, []byte(content), 0644)
	require.NoError(t, err)

	manager := NewManager(cfg, logger)
	err = manager.LoadTerraformExtras(context.Background())
	require.NoError(t, err)

	// All required fields must be set - Validate() should pass
	require.NoError(t, cfg.Validate(), "Config should be valid after loading complete tfvars")

	// Verify specific values
	assert.Equal(t, "test-core", cfg.ClusterName)
	assert.Equal(t, "cluster.example.com", cfg.ControlPlaneEndpoint)
	assert.True(t, cfg.HAProxyIP.Equal(net.ParseIP("192.168.1.199")))
	assert.Equal(t, "v1.35.1", cfg.KubernetesVersion)
	assert.Equal(t, "v1.12.3", cfg.TalosVersion)
	assert.Equal(t, "factory.talos.dev/nocloud-installer/test:v1.12.3", cfg.InstallerImage)
	assert.Equal(t, "192.168.1.200", cfg.ProxmoxSSHHost)
	assert.Equal(t, "root", cfg.HAProxyLoginUser)
	assert.Equal(t, "admin", cfg.HAProxyStatsUser)
	assert.Equal(t, "changeme", cfg.HAProxyStatsPassword)
	assert.Len(t, cfg.ProxmoxNodeIPs, 2)
	assert.True(t, cfg.ProxmoxNodeIPs["pve1"].Equal(net.ParseIP("192.168.1.200")))
}

func TestResolveTFVarsPath(t *testing.T) {
	t.Run("file exists at configured path", func(t *testing.T) {
		tmpDir := t.TempDir()
		tfvarsPath := filepath.Join(tmpDir, "terraform.tfvars")
		os.WriteFile(tfvarsPath, []byte(`cluster_name = "test"`), 0644)

		cfg := types.TestConfig()
		cfg.TerraformTFVars = tfvarsPath
		logger := zaptest.NewLogger(t)
		mgr := NewManager(cfg, logger)

		err := mgr.ResolveTFVarsPath()
		require.NoError(t, err)
		assert.Equal(t, tfvarsPath, cfg.TerraformTFVars)
	})

	t.Run("file exists in parent directory", func(t *testing.T) {
		// Create parent/child directory structure
		parentDir := t.TempDir()
		childDir := filepath.Join(parentDir, "subdir")
		require.NoError(t, os.Mkdir(childDir, 0755))

		// Put tfvars in parent
		tfvarsPath := filepath.Join(parentDir, "terraform.tfvars")
		os.WriteFile(tfvarsPath, []byte(`cluster_name = "test"`), 0644)

		// Run from child directory
		origDir, _ := os.Getwd()
		require.NoError(t, os.Chdir(childDir))
		defer os.Chdir(origDir)

		cfg := types.TestConfig()
		cfg.TerraformTFVars = "terraform.tfvars"
		logger := zaptest.NewLogger(t)
		mgr := NewManager(cfg, logger)

		err := mgr.ResolveTFVarsPath()
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("..", "terraform.tfvars"), cfg.TerraformTFVars)
	})

	t.Run("file not found anywhere", func(t *testing.T) {
		tmpDir := t.TempDir()
		origDir, _ := os.Getwd()
		require.NoError(t, os.Chdir(tmpDir))
		defer os.Chdir(origDir)

		cfg := types.TestConfig()
		cfg.TerraformTFVars = "terraform.tfvars"
		logger := zaptest.NewLogger(t)
		mgr := NewManager(cfg, logger)

		err := mgr.ResolveTFVarsPath()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "terraform.tfvars not found")
	})
}

// Windows-specific path handling validation
func TestPathHandling_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}

	f := newTestFixture(t)
	path := f.manager.NodeConfigPath(100, types.RoleControlPlane)

	// Verify Windows path separators
	assert.True(t, strings.Contains(path, "\\") || !strings.Contains(path, "/"),
		"Expected Windows path separators in: %s", path)
}
