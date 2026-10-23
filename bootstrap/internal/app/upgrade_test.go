package app

import (
	"context"
	"errors"
	"net"
	"sort"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// testLogger returns a test-scoped zap logger that writes to the test output buffer.
func testLogger(t *testing.T) *zap.Logger {
	return zaptest.NewLogger(t)
}

var errUpgradeFailed = errors.New("upgrade failed on node")

// TestBuildUpgradePlan_OrderingAndFiltering verifies that the upgrade plan puts
// control planes before workers, sorts each group by VMID, and respects the
// --nodes IP filter.
func TestBuildUpgradePlan_OrderingAndFiltering(t *testing.T) {
	deployed := &types.ClusterState{
		ControlPlanes: []types.NodeState{
			{VMID: 202, IP: net.ParseIP("192.168.1.22")},
			{VMID: 201, IP: net.ParseIP("192.168.1.21")},
		},
		Workers: []types.NodeState{
			{VMID: 302, IP: net.ParseIP("192.168.1.42")},
			{VMID: 301, IP: net.ParseIP("192.168.1.41")},
		},
	}

	t.Run("all nodes, no filter — CPs first, sorted by VMID", func(t *testing.T) {
		plan := buildUpgradePlan(deployed, nil)

		require.Len(t, plan, 4)
		assert.Equal(t, types.VMID(201), plan[0].VMID)
		assert.Equal(t, types.VMID(202), plan[1].VMID)
		assert.Equal(t, types.VMID(301), plan[2].VMID)
		assert.Equal(t, types.VMID(302), plan[3].VMID)

		assert.Equal(t, types.RoleControlPlane, plan[0].Role)
		assert.Equal(t, types.RoleControlPlane, plan[1].Role)
		assert.Equal(t, types.RoleWorker, plan[2].Role)
		assert.Equal(t, types.RoleWorker, plan[3].Role)
	})

	t.Run("IP filter restricts to matching nodes only", func(t *testing.T) {
		filter := []net.IP{net.ParseIP("192.168.1.21"), net.ParseIP("192.168.1.41")}
		plan := buildUpgradePlan(deployed, filter)

		require.Len(t, plan, 2)
		assert.Equal(t, types.VMID(201), plan[0].VMID)
		assert.Equal(t, types.VMID(301), plan[1].VMID)
	})

	t.Run("filter with no matching IPs returns empty plan", func(t *testing.T) {
		filter := []net.IP{net.ParseIP("10.0.0.1")}
		plan := buildUpgradePlan(deployed, filter)
		assert.Empty(t, plan)
	})

	t.Run("empty deployed state returns empty plan", func(t *testing.T) {
		plan := buildUpgradePlan(&types.ClusterState{}, nil)
		assert.Empty(t, plan)
	})
}

// TestBuildUpgradePlan_OnlyCPs verifies a cluster with only control planes works.
func TestBuildUpgradePlan_OnlyCPs(t *testing.T) {
	deployed := &types.ClusterState{
		ControlPlanes: []types.NodeState{
			{VMID: 201, IP: net.ParseIP("192.168.1.201")},
		},
	}
	plan := buildUpgradePlan(deployed, nil)

	require.Len(t, plan, 1)
	assert.Equal(t, types.RoleControlPlane, plan[0].Role)
}

// TestBuildUpgradePlan_SortingStability verifies that the sort is stable and
// deterministic even when VMIDs are not pre-sorted.
func TestBuildUpgradePlan_SortingStability(t *testing.T) {
	deployed := &types.ClusterState{
		ControlPlanes: []types.NodeState{
			{VMID: 205, IP: net.ParseIP("192.168.1.25")},
			{VMID: 201, IP: net.ParseIP("192.168.1.21")},
			{VMID: 203, IP: net.ParseIP("192.168.1.23")},
		},
		Workers: []types.NodeState{
			{VMID: 305, IP: net.ParseIP("192.168.1.45")},
			{VMID: 301, IP: net.ParseIP("192.168.1.41")},
		},
	}

	plan := buildUpgradePlan(deployed, nil)

	require.Len(t, plan, 5)

	// Verify CPs are sorted
	assert.True(t, sort.SliceIsSorted(plan[:3], func(i, j int) bool {
		return plan[i].VMID < plan[j].VMID
	}))

	// Verify workers are sorted
	assert.True(t, sort.SliceIsSorted(plan[3:], func(i, j int) bool {
		return plan[i].VMID < plan[j].VMID
	}))
}

// TestParseNodeIPs_ValidAndInvalid verifies IP list parsing from CLI flags.
func TestParseNodeIPs_ValidAndInvalid(t *testing.T) {
	t.Run("valid comma-separated IPs", func(t *testing.T) {
		ips, err := parseNodeIPs("192.168.1.201,192.168.1.202")
		require.NoError(t, err)
		assert.Len(t, ips, 2)
	})

	t.Run("empty string returns nil", func(t *testing.T) {
		ips, err := parseNodeIPs("")
		require.NoError(t, err)
		assert.Nil(t, ips)
	})

	t.Run("invalid IP returns error", func(t *testing.T) {
		_, err := parseNodeIPs("not-an-ip")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid IP")
	})

	t.Run("mix of valid and invalid returns error", func(t *testing.T) {
		_, err := parseNodeIPs("192.168.1.201,bad")
		require.Error(t, err)
	})
}

// TestVersionMatchesTarget verifies the version comparison helper.
func TestVersionMatchesTarget(t *testing.T) {
	tests := []struct {
		nodeVersion string
		targetImage string
		want        bool
	}{
		// Image tag matches reported version exactly
		{"v1.13.4", "factory.talos.dev/installer:v1.13.4", true},
		// Tag mismatch
		{"v1.12.3", "factory.talos.dev/installer:v1.13.4", false},
		// Empty node version (unreachable) — never matches
		{"", "factory.talos.dev/installer:v1.13.4", false},
		// Empty image tag
		{"v1.13.4", "factory.talos.dev/installer:", false},
		// Image with SHA digest — extract tag only
		{"v1.13.4", "factory.talos.dev/installer:v1.13.4@sha256:abc", true},
		// Node version with "v" prefix vs image tag without
		{"v1.13.4", "factory.talos.dev/installer/ext:1.13.4", false},
	}

	for _, tt := range tests {
		t.Run(tt.nodeVersion+"_vs_"+tt.targetImage, func(t *testing.T) {
			got := versionMatchesTarget(tt.nodeVersion, tt.targetImage)
			assert.Equal(t, tt.want, got, "nodeVersion=%s targetImage=%s", tt.nodeVersion, tt.targetImage)
		})
	}
}

// TestRunUpgrade_DryRunExitsEarly verifies that dry-run mode prints the plan
// and returns nil without making any API calls.
func TestRunUpgrade_DryRunExitsEarly(t *testing.T) {
	cfg := types.TestConfig()
	cfg.DryRun = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes: []types.NodeState{
			{VMID: 201, IP: net.ParseIP("192.168.1.21")},
		},
		Workers: []types.NodeState{
			{VMID: 301, IP: net.ParseIP("192.168.1.41")},
		},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4",
		&mockUpgradeDeps{})
	require.NoError(t, err)
}

// TestRunUpgrade_PlanModeExitsEarly verifies that plan mode is also a no-op.
func TestRunUpgrade_PlanModeExitsEarly(t *testing.T) {
	cfg := types.TestConfig()
	cfg.PlanMode = true
	cfg.DryRun = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes:      []types.NodeState{{VMID: 201, IP: net.ParseIP("192.168.1.21")}},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4",
		&mockUpgradeDeps{})
	require.NoError(t, err)
}

// TestRunUpgrade_SkipsNodeAlreadyAtVersion verifies that a node already at
// the target version is skipped without triggering an upgrade call.
func TestRunUpgrade_SkipsNodeAlreadyAtVersion(t *testing.T) {
	cfg := types.TestConfig()
	cfg.AutoApprove = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{
			"192.168.1.21": "v1.13.4", // already at target
		},
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes:      []types.NodeState{{VMID: 201, IP: net.ParseIP("192.168.1.21")}},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4", deps)
	require.NoError(t, err)
	assert.Equal(t, 0, deps.upgradeCount, "node already at target version should not trigger upgrade")
}

// TestRunUpgrade_UpgradesNodeNotAtVersion verifies that a node behind the target
// version gets an upgrade call followed by the wait sequence.
func TestRunUpgrade_UpgradesNodeNotAtVersion(t *testing.T) {
	cfg := types.TestConfig()
	cfg.AutoApprove = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{
			"192.168.1.21": "v1.12.3", // behind target
		},
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes:      []types.NodeState{{VMID: 201, IP: net.ParseIP("192.168.1.21")}},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4", deps)
	require.NoError(t, err)
	assert.Equal(t, 1, deps.upgradeCount, "node behind target should trigger exactly one upgrade call")
}

// TestRunUpgrade_WorkerCordonsAndUncordons verifies that worker upgrade calls
// cordon before and uncordon after the upgrade.
func TestRunUpgrade_WorkerCordonsAndUncordons(t *testing.T) {
	cfg := types.TestConfig()
	cfg.AutoApprove = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{
			"192.168.1.41": "v1.12.3", // worker, behind target
		},
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		Workers:            []types.NodeState{{VMID: 301, IP: net.ParseIP("192.168.1.41")}},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4", deps)
	require.NoError(t, err)
	assert.Equal(t, 1, deps.cordonCount, "worker should be cordoned before upgrade")
	assert.Equal(t, 1, deps.uncordonCount, "worker should be uncordoned after upgrade")
}

// TestRunUpgrade_AbortOnFirstNodeFailure verifies that a failing upgrade on the
// first node aborts the entire run and returns an error.
func TestRunUpgrade_AbortOnFirstNodeFailure(t *testing.T) {
	cfg := types.TestConfig()
	cfg.AutoApprove = true

	a := &App{
		Cfg:    cfg,
		Logger: testLogger(t),
	}

	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{
			"192.168.1.21": "v1.12.3",
			"192.168.1.22": "v1.12.3",
		},
		upgradeError: map[string]bool{
			"192.168.1.21": true, // first CP fails
		},
	}

	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes: []types.NodeState{
			{VMID: 201, IP: net.ParseIP("192.168.1.21")},
			{VMID: 202, IP: net.ParseIP("192.168.1.22")},
		},
	}

	err := a.runUpgradeWithState(context.Background(), deployed, nil, "factory.talos.dev/installer:v1.13.4", deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback")
	// Second node should NOT have been upgraded
	assert.Equal(t, 1, deps.upgradeCount, "upgrade should abort after first failure")
}

// mockUpgradeDeps is a test double for the Talos+kubectl API calls made during upgrade.
type mockUpgradeDeps struct {
	nodeVersions  map[string]string
	upgradeError  map[string]bool
	upgradeCount  int
	cordonCount   int
	uncordonCount int
}

func (m *mockUpgradeDeps) NodeVersion(_ context.Context, ip net.IP) (string, error) {
	if v, ok := m.nodeVersions[ip.String()]; ok {
		return v, nil
	}
	return "", nil
}

func (m *mockUpgradeDeps) Upgrade(_ context.Context, ip net.IP, _ string) error {
	m.upgradeCount++
	if m.upgradeError != nil && m.upgradeError[ip.String()] {
		return errUpgradeFailed
	}
	return nil
}

func (m *mockUpgradeDeps) WaitForAPI(_ context.Context, _ net.IP) error { return nil }

func (m *mockUpgradeDeps) WaitForEtcdHealthy(_ context.Context, _ net.IP, _ interface{}) error {
	return nil
}

func (m *mockUpgradeDeps) GetNodeNameByIP(_ context.Context, ip net.IP) (string, error) {
	return "node-" + ip.String(), nil
}

func (m *mockUpgradeDeps) WaitForNodeReady(_ context.Context, _ string, _ interface{}) error {
	return nil
}

func (m *mockUpgradeDeps) CordonNode(_ context.Context, _ string) error {
	m.cordonCount++
	return nil
}

func (m *mockUpgradeDeps) UncordonNode(_ context.Context, _ string) error {
	m.uncordonCount++
	return nil
}

func (m *mockUpgradeDeps) WaitForLonghornVolumesHealthy(_ context.Context, _ interface{}) error {
	return nil
}
