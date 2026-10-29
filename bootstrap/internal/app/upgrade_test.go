package app

import (
	"context"
	"errors"
	"net"
	"os"
	"sort"
	"testing"
	"time"

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
		// Registry port is not the tag
		{"v1.13.4", "registry.lan:5000/installer:v1.13.4", true},
		{"v1.13.4", "registry.lan:5000/installer", false},
		// Image Factory schematic path
		{"v1.13.4", "factory.talos.dev/nocloud-installer/0123abcd:v1.13.4", true},
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

// TestMain shortens the post-upgrade poll so tests do not sleep.
func TestMain(m *testing.M) {
	upgradePollInterval = time.Millisecond
	os.Exit(m.Run())
}

func singleWorkerState() *types.ClusterState {
	return &types.ClusterState{
		BootstrapCompleted: true,
		Workers:            []types.NodeState{{VMID: 301, IP: net.ParseIP("192.168.1.41")}},
	}
}

func twoCPState() *types.ClusterState {
	return &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes: []types.NodeState{
			{VMID: 201, IP: net.ParseIP("192.168.1.21")},
			{VMID: 202, IP: net.ParseIP("192.168.1.22")},
		},
	}
}

func autoApproveApp(t *testing.T) *App {
	cfg := types.TestConfig()
	cfg.AutoApprove = true
	return &App{Cfg: cfg, Logger: testLogger(t)}
}

const testTargetImage = "factory.talos.dev/installer:v1.13.4"

// TestRunUpgrade_WaitsForRebootIntoTarget verifies the run does not move on
// while the node still reports the old version, which is what it does between
// accepting the upgrade and rebooting.
func TestRunUpgrade_WaitsForRebootIntoTarget(t *testing.T) {
	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{"192.168.1.21": "v1.12.3", "192.168.1.22": "v1.12.3"},
		oldPolls:     3,
	}

	err := autoApproveApp(t).runUpgradeWithState(context.Background(), twoCPState(), nil, testTargetImage, deps)
	require.NoError(t, err)
	assert.Equal(t, 2, deps.upgradeCount)
	assert.GreaterOrEqual(t, deps.versionPolls["192.168.1.21"], 4,
		"must keep polling until the node reports the target tag")
}

// TestRunUpgrade_AbortsWhenNodeNeverReportsTarget verifies a node that never
// reboots into the target fails the run instead of letting the next node go.
func TestRunUpgrade_AbortsWhenNodeNeverReportsTarget(t *testing.T) {
	saved := upgradeNodeTimeout
	upgradeNodeTimeout = 50 * time.Millisecond
	defer func() { upgradeNodeTimeout = saved }()

	deps := &mockUpgradeDeps{
		nodeVersions: map[string]string{"192.168.1.21": "v1.12.3", "192.168.1.22": "v1.12.3"},
		stuck:        true,
	}

	err := autoApproveApp(t).runUpgradeWithState(context.Background(), twoCPState(), nil, testTargetImage, deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not report the target version")
	assert.Equal(t, 1, deps.upgradeCount, "second control plane must not be upgraded")
}

// TestRunUpgrade_GateFailuresAbort verifies every post-upgrade gate is fatal:
// moving on to the next node after a failed gate is how quorum or the last
// good Longhorn replica is lost.
func TestRunUpgrade_GateFailuresAbort(t *testing.T) {
	tests := []struct {
		name     string
		state    *types.ClusterState
		mutate   func(*mockUpgradeDeps)
		wantErr  string
		upgrades int
	}{
		{"node name unresolvable aborts before upgrading", twoCPState(),
			func(m *mockUpgradeDeps) { m.nodeNameErr = errors.New("kubectl down") }, "resolve kubernetes node name", 0},
		{"etcd not healthy", twoCPState(),
			func(m *mockUpgradeDeps) { m.etcdErr = errors.New("timeout") }, "etcd not healthy", 1},
		{"node not Ready", twoCPState(),
			func(m *mockUpgradeDeps) { m.readyErr = errors.New("timeout") }, "not Ready", 1},
		{"uncordon fails", singleWorkerState(),
			func(m *mockUpgradeDeps) { m.uncordonErr = errors.New("forbidden") }, "uncordon worker", 1},
		{"longhorn degraded", singleWorkerState(),
			func(m *mockUpgradeDeps) { m.longhornErr = errors.New("2 degraded") }, "longhorn volumes degraded", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := &mockUpgradeDeps{nodeVersions: map[string]string{
				"192.168.1.21": "v1.12.3", "192.168.1.22": "v1.12.3", "192.168.1.41": "v1.12.3",
			}}
			tt.mutate(deps)

			err := autoApproveApp(t).runUpgradeWithState(context.Background(), tt.state, nil, testTargetImage, deps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, tt.upgrades, deps.upgradeCount)
		})
	}
}

// TestRunUpgrade_RejectsUntaggedImage verifies an image the run cannot verify
// a reboot against is refused before any node, plan or audit entry.
func TestRunUpgrade_RejectsUntaggedImage(t *testing.T) {
	for _, image := range []string{
		"factory.talos.dev/installer@sha256:abc",
		"registry.lan:5000/installer",
		"factory.talos.dev/installer:",
	} {
		t.Run(image, func(t *testing.T) {
			deps := &mockUpgradeDeps{}
			err := autoApproveApp(t).runUpgradeWithState(context.Background(), twoCPState(), nil, image, deps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "has no tag")
			assert.Zero(t, deps.upgradeCount)
			assert.Empty(t, deps.versionPolls, "no node may be contacted")
		})
	}
}

// TestRunUpgrade_EtcdGateQueriesUpgradedNode verifies etcd is checked through
// the upgraded member, expecting every control plane, not through a peer that
// would answer while that member is still down.
func TestRunUpgrade_EtcdGateQueriesUpgradedNode(t *testing.T) {
	deps := &mockUpgradeDeps{nodeVersions: map[string]string{"192.168.1.21": "v1.12.3", "192.168.1.22": "v1.12.3"}}

	err := autoApproveApp(t).runUpgradeWithState(context.Background(), twoCPState(), nil, testTargetImage, deps)
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.21", "192.168.1.22"}, deps.etcdIPs)
	assert.Equal(t, 2, deps.etcdExpected)
}

// mockUpgradeDeps is a test double for the Talos+kubectl API calls made during
// upgrade. A successful Upgrade makes NodeVersion report the target tag after
// oldPolls further polls, unless stuck is set.
type mockUpgradeDeps struct {
	nodeVersions  map[string]string
	upgradeError  map[string]bool
	upgradeCount  int
	cordonCount   int
	uncordonCount int

	oldPolls     int
	stuck        bool
	versionPolls map[string]int
	upgraded     map[string]int

	nodeNameErr error
	etcdErr     error
	readyErr    error
	uncordonErr error
	longhornErr error

	etcdIPs      []string
	etcdExpected int
}

func (m *mockUpgradeDeps) NodeVersion(_ context.Context, ip net.IP) (string, error) {
	key := ip.String()
	if m.versionPolls == nil {
		m.versionPolls = map[string]int{}
	}
	m.versionPolls[key]++
	if start, ok := m.upgraded[key]; ok && !m.stuck && m.versionPolls[key]-start > m.oldPolls {
		return "v1.13.4", nil
	}
	if v, ok := m.nodeVersions[key]; ok {
		return v, nil
	}
	return "", nil
}

func (m *mockUpgradeDeps) Upgrade(_ context.Context, ip net.IP, _ string) error {
	m.upgradeCount++
	if m.upgradeError != nil && m.upgradeError[ip.String()] {
		return errUpgradeFailed
	}
	if m.upgraded == nil {
		m.upgraded = map[string]int{}
	}
	if m.versionPolls == nil {
		m.versionPolls = map[string]int{}
	}
	m.upgraded[ip.String()] = m.versionPolls[ip.String()]
	return nil
}

func (m *mockUpgradeDeps) WaitForEtcdMembers(_ context.Context, ip net.IP, expected int, _ time.Duration) error {
	m.etcdIPs = append(m.etcdIPs, ip.String())
	m.etcdExpected = expected
	return m.etcdErr
}

func (m *mockUpgradeDeps) GetNodeNameByIP(_ context.Context, ip net.IP) (string, error) {
	if m.nodeNameErr != nil {
		return "", m.nodeNameErr
	}
	return "node-" + ip.String(), nil
}

func (m *mockUpgradeDeps) WaitForNodeReady(_ context.Context, _ string, _ time.Duration) error {
	return m.readyErr
}

func (m *mockUpgradeDeps) CordonNode(_ context.Context, _ string) error {
	m.cordonCount++
	return nil
}

func (m *mockUpgradeDeps) UncordonNode(_ context.Context, _ string) error {
	m.uncordonCount++
	return m.uncordonErr
}

func (m *mockUpgradeDeps) WaitForLonghornVolumesHealthy(_ context.Context, _ time.Duration) error {
	return m.longhornErr
}

func TestRunUpgrade_RejectsUnknownNodeIP(t *testing.T) {
	a := &App{Cfg: types.TestConfig(), Logger: testLogger(t)}
	deployed := &types.ClusterState{
		BootstrapCompleted: true,
		ControlPlanes:      []types.NodeState{{VMID: 201, IP: net.ParseIP("192.168.1.21")}},
	}

	for name, filter := range map[string][]net.IP{
		"all mistyped": {net.ParseIP("192.168.1.99")},
		"one mistyped": {net.ParseIP("192.168.1.21"), net.ParseIP("192.168.1.99")},
	} {
		t.Run(name, func(t *testing.T) {
			err := a.runUpgradeWithState(context.Background(), deployed, filter,
				"factory.talos.dev/installer:v1.13.4", &mockUpgradeDeps{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "192.168.1.99")
		})
	}
}
