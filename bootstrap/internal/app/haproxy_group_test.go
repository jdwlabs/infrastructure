package app

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMember stands in for one instance's SSH client.
type fakeMember struct {
	unreachable error
	keepalived  string
	holds       bool
	deployed    string
	readErr     error
	pushErr     error
	pushed      []string
}

func (f *fakeMember) CheckConnectivity() error { return f.unreachable }

func (f *fakeMember) ServiceState(context.Context) (string, error) { return "active", nil }

func (f *fakeMember) KeepalivedState(context.Context) (string, error) {
	if f.keepalived == "" {
		return "active", nil
	}
	return f.keepalived, nil
}

func (f *fakeMember) HoldsAddress(context.Context, net.IP) (bool, error) { return f.holds, nil }

func (f *fakeMember) Stats(context.Context) ([]haproxy.ServerStat, error) {
	return haproxy.ParseStats("# svname,status,addr,check_status,\ntalos-cp-201,UP,192.168.1.21:6443,L4OK,\n")
}

func (f *fakeMember) DeployedConfig(context.Context) (string, error) {
	return f.deployed, f.readErr
}

func (f *fakeMember) Update(_ context.Context, config string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = append(f.pushed, config)
	f.deployed = config
	return nil
}

func pairTestContext(t *testing.T) (*haproxyContext, string) {
	t.Helper()
	hc := haproxyTestContext()
	hc.cfg.HAProxyVMs = []types.HAProxyVM{
		{Name: "haproxy-1", Node: "pve1", VMID: 110, IP: "192.168.1.11"},
		{Name: "haproxy-2", Node: "pve5", VMID: 113, IP: "192.168.1.12"},
	}
	group, err := haproxy.ResolveGroup(hc.cfg, "")
	require.NoError(t, err)
	hc.group = group
	hc.host = ""

	rendered, _, failure := hc.renderConfig()
	require.Nil(t, failure)
	return hc, rendered
}

func connectTo(members map[string]*fakeMember) connectMember {
	return func(inst haproxy.Instance) (groupMember, *haproxy.Failure) {
		return members[inst.Name], nil
	}
}

func noVM(string) (haproxy.VMInfo, []string) {
	return haproxy.VMInfo{State: "running", Source: "terraform"}, nil
}

func noProgress(string, haproxy.GroupApplyResult) {}

// Every instance binds the virtual address, so the pair serves one config and
// a backup accepts connections the moment the address arrives.
func TestGroupRendersOneConfigBoundToTheVirtualAddress(t *testing.T) {
	_, rendered := pairTestContext(t)

	assert.Contains(t, rendered, "bind 192.168.1.199:6443")
	assert.NotContains(t, rendered, "192.168.1.11")
	assert.NotContains(t, rendered, "192.168.1.12")
}

func TestInspectGroupReportsEachInstanceAndTheHolder(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered},
		"haproxy-2": {deployed: rendered, holds: true},
	}

	res := inspectGroup(context.Background(), hc, connectTo(members), noVM)

	require.Nil(t, res.Failure)
	assert.Equal(t, "192.168.1.199", res.VirtualAddress)
	assert.Equal(t, "haproxy-2", res.Holder)
	require.Len(t, res.Instances, 2)
	assert.Equal(t, "false", res.Instances[0].HoldsAddress)
	assert.Equal(t, "true", res.Instances[1].HoldsAddress)
	assert.Equal(t, "false", res.Instances[0].ConfigDrift)
	assert.Empty(t, groupStatusHelp(res), "a healthy pair needs no next step")
}

// The outage this exists for: one host gone. The survivor must still be read
// and named as the holder, and the report must still fail.
func TestInspectGroupKeepsReadingWhenOneInstanceIsUnreachable(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {unreachable: errors.New("dial SSH: i/o timeout")},
		"haproxy-2": {deployed: rendered, holds: true},
	}

	res := inspectGroup(context.Background(), hc, connectTo(members), noVM)

	assert.Equal(t, "haproxy-2", res.Holder)
	assert.Equal(t, "unreachable", res.Instances[0].SSH)
	assert.Equal(t, haproxy.Unknown, res.Instances[0].HoldsAddress)
	assert.Equal(t, "ok", res.Instances[1].SSH)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "instance_unreachable", res.Failure.Code)
	assert.Contains(t, res.Notes[0], "haproxy-1: SSH to")
	assert.NotEmpty(t, groupStatusHelp(res))
}

func TestInspectGroupShowsAnInstanceServingAStaleConfig(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered, holds: true},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := inspectGroup(context.Background(), hc, connectTo(members), noVM)

	assert.Equal(t, "false", res.Instances[0].ConfigDrift)
	assert.Equal(t, "true", res.Instances[1].ConfigDrift)
	help := groupStatusHelp(res)
	require.NotEmpty(t, help)
	assert.Contains(t, help[0], "talops haproxy plan")
}

func TestPlanGroupDiffsEachInstanceSeparately(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := planGroup(context.Background(), hc, connectTo(members), false)

	require.Nil(t, res.Failure)
	assert.True(t, res.Drift)
	assert.Equal(t, "false", res.Instances[0].Drift)
	assert.Equal(t, "true", res.Instances[1].Drift)
	assert.Equal(t, res.RenderedHash, res.Instances[0].DeployedHash)
	assert.NotEmpty(t, res.Instances[1].Diff)
	assert.Empty(t, members["haproxy-2"].pushed, "plan never writes")
}

// "No drift" must not be the answer for an instance that was never read.
func TestPlanGroupFailsWhenAnInstanceCannotBeRead(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered},
		"haproxy-2": {readErr: errors.New("dial SSH: connection refused")},
	}

	res := planGroup(context.Background(), hc, connectTo(members), false)

	assert.Equal(t, haproxy.Unknown, res.Instances[1].Drift)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "config_unreadable", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-2")
}

func TestApplyGroupPushesToEveryDriftingInstance(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: "# stale\n"},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), false, noProgress)

	require.Nil(t, res.Failure)
	assert.True(t, res.Changed)
	assert.Equal(t, []string{rendered}, members["haproxy-1"].pushed)
	assert.Equal(t, []string{rendered}, members["haproxy-2"].pushed)
}

// Retrying after a partial failure must not reload the instance that already
// took the config.
func TestApplyGroupSkipsAnInstanceThatIsAlreadyCurrent(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), false, noProgress)

	require.Nil(t, res.Failure)
	assert.Empty(t, members["haproxy-1"].pushed)
	assert.Equal(t, []string{rendered}, members["haproxy-2"].pushed)
	assert.False(t, res.Instances[0].Changed)
	assert.True(t, res.Instances[1].Changed)
}

// The failure a pair must never hide: the first instance takes the config, the
// second does not, and the command would otherwise report success.
func TestApplyGroupReportsDivergenceWhenOneInstanceFails(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: "# stale\n"},
		"haproxy-2": {deployed: "# stale\n", pushErr: errors.New("dial SSH: i/o timeout")},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), false, noProgress)

	assert.Equal(t, []string{rendered}, members["haproxy-1"].pushed)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "group_divergent", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-2")
	assert.Equal(t, "dial SSH: i/o timeout", res.Instances[1].Error)
	assert.NotEmpty(t, groupApplyHelp(res))
}

// The failing instance comes first here. Stopping at it would leave the
// reachable one — the one serving — on stale backends.
func TestApplyGroupStillPushesToLaterInstancesAfterAFailure(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {readErr: errors.New("dial SSH: i/o timeout"), pushErr: errors.New("dial SSH: i/o timeout")},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), false, noProgress)

	assert.Equal(t, []string{rendered}, members["haproxy-2"].pushed)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "group_divergent", res.Failure.Code)
}

func TestApplyGroupDryRunPushesNothing(t *testing.T) {
	hc, _ := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: "# stale\n"},
		"haproxy-2": {deployed: "# stale\n"},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), true, noProgress)

	require.Nil(t, res.Failure)
	assert.True(t, res.Drift)
	assert.False(t, res.Changed)
	assert.Empty(t, members["haproxy-1"].pushed)
	assert.Empty(t, members["haproxy-2"].pushed)
}

func TestApplyGroupIsANoOpOnAConvergedPair(t *testing.T) {
	hc, rendered := pairTestContext(t)
	members := map[string]*fakeMember{
		"haproxy-1": {deployed: rendered},
		"haproxy-2": {deployed: rendered},
	}

	res := applyGroup(context.Background(), hc, connectTo(members), false, noProgress)

	require.Nil(t, res.Failure)
	assert.False(t, res.Changed)
	assert.False(t, res.Drift)
}

// Asking about the virtual address itself reaches whichever instance holds it.
// Calling that "unmanaged" would send the operator to the rebuild runbook for
// a pair that is fully declared.
func TestDescribeHAProxyVMNamesTheVirtualAddressForWhatItIs(t *testing.T) {
	hc, _ := pairTestContext(t)

	vm, notes := (&App{}).describeHAProxyVM(hc.cfg, "192.168.1.199", nil)

	assert.Equal(t, "virtual", vm.Source)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "virtual address")
	assert.Empty(t, vmHelp(vm))
}

func TestEveryGroupFailureNamesAWayForward(t *testing.T) {
	for _, code := range []string{
		"haproxy_group_invalid", "virtual_address_split", "virtual_address_unheld",
		"instance_unreachable", "instance_unread", "keepalived_inactive", "group_divergent",
	} {
		assert.NotEmpty(t, helpForFailure(&haproxy.Failure{Code: code}), code)
	}
}
