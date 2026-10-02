package haproxy

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func groupConfig(vms ...types.HAProxyVM) *types.Config {
	cfg := types.DefaultConfig()
	cfg.HAProxyIP = net.ParseIP("192.168.1.199")
	cfg.HAProxyVMs = vms
	return cfg
}

var (
	lbOne = types.HAProxyVM{Name: "haproxy-1", Node: "pve1", VMID: 110, IP: "192.168.1.11"}
	lbTwo = types.HAProxyVM{Name: "haproxy-2", Node: "pve5", VMID: 113, IP: "192.168.1.12"}
)

// The live state today: one declared VM whose own address is the endpoint.
func TestResolveGroupKeepsAStandaloneLoadBalancerOnItsEndpointAddress(t *testing.T) {
	standalone := types.HAProxyVM{Name: "haproxy-1", Node: "pve1", VMID: 110, IP: "192.168.1.199"}

	group, err := ResolveGroup(groupConfig(standalone), "")

	require.NoError(t, err)
	assert.False(t, group.Virtual)
	assert.Equal(t, "192.168.1.199", group.BindAddress.String())
	assert.Equal(t, []Instance{{Name: "haproxy-1", Host: "192.168.1.199"}}, group.Instances)
}

func TestResolveGroupTargetsTheEndpointWhenNoVMIsDeclared(t *testing.T) {
	group, err := ResolveGroup(groupConfig(), "")

	require.NoError(t, err)
	assert.False(t, group.Virtual)
	assert.Equal(t, []Instance{{Host: "192.168.1.199"}}, group.Instances)
}

// Two declared VMs make the endpoint a virtual address: every instance is
// pushed to on its own address, and every one binds the shared address.
func TestResolveGroupTargetsEveryInstanceOfAPairAndBindsTheVirtualAddress(t *testing.T) {
	group, err := ResolveGroup(groupConfig(lbOne, lbTwo), "")

	require.NoError(t, err)
	assert.True(t, group.Virtual)
	assert.Equal(t, "192.168.1.199", group.BindAddress.String())
	assert.Equal(t, []Instance{
		{Name: "haproxy-1", Host: "192.168.1.11"},
		{Name: "haproxy-2", Host: "192.168.1.12"},
	}, group.Instances)
}

// Staging one member before it joins: it must get the config it will serve
// once it holds the virtual address, not one bound to its own.
func TestResolveGroupHostOverrideOnAPairMemberStillBindsTheVirtualAddress(t *testing.T) {
	group, err := ResolveGroup(groupConfig(lbOne, lbTwo), "192.168.1.12")

	require.NoError(t, err)
	assert.False(t, group.Virtual, "one instance was asked for, so one is reported")
	assert.Equal(t, "192.168.1.199", group.BindAddress.String())
	assert.Equal(t, []Instance{{Name: "haproxy-2", Host: "192.168.1.12"}}, group.Instances)
}

// The rebuild runbook verifies a replacement on a temporary address that no
// declaration names. It has to bind that address or it serves nothing there.
func TestResolveGroupHostOverrideOnAnUndeclaredAddressBindsThatAddress(t *testing.T) {
	for name, cfg := range map[string]*types.Config{
		"standalone": groupConfig(types.HAProxyVM{Name: "haproxy-1", VMID: 110, IP: "192.168.1.199"}),
		"pair":       groupConfig(lbOne, lbTwo),
	} {
		t.Run(name, func(t *testing.T) {
			group, err := ResolveGroup(cfg, "192.168.1.77")

			require.NoError(t, err)
			assert.Equal(t, "192.168.1.77", group.BindAddress.String())
			assert.Equal(t, []Instance{{Host: "192.168.1.77"}}, group.Instances)
		})
	}
}

func TestResolveGroupRefusesAPairMemberWithoutAnAddress(t *testing.T) {
	broken := lbTwo
	broken.IP = ""

	_, err := ResolveGroup(groupConfig(lbOne, broken), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "haproxy-2")
}

// Pushing to the virtual address reaches whichever instance holds it and
// silently skips the other, which is the divergence the pair must not have.
func TestResolveGroupRefusesAPairMemberOnTheVirtualAddress(t *testing.T) {
	onVIP := lbOne
	onVIP.IP = "192.168.1.199"

	_, err := ResolveGroup(groupConfig(onVIP, lbTwo), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "virtual address")
}

func TestResolveGroupRefusesWithoutAnyAddress(t *testing.T) {
	cfg := groupConfig()
	cfg.HAProxyIP = nil

	_, err := ResolveGroup(cfg, "")
	require.Error(t, err)
}

type fakePusher struct {
	err    error
	pushed []string
}

func (f *fakePusher) Update(_ context.Context, config string) error {
	if f.err != nil {
		return f.err
	}
	f.pushed = append(f.pushed, config)
	return nil
}

func pushersFor(pushers map[string]*fakePusher) func(Instance) (Pusher, error) {
	return func(inst Instance) (Pusher, error) {
		p, ok := pushers[inst.Name]
		if !ok {
			return nil, errors.New("no SSH auth available")
		}
		return p, nil
	}
}

var pair = []Instance{{Name: "haproxy-1", Host: "192.168.1.11"}, {Name: "haproxy-2", Host: "192.168.1.12"}}

func TestPushToAllInstallsTheSameConfigOnEveryInstance(t *testing.T) {
	one, two := &fakePusher{}, &fakePusher{}

	outcomes := PushToAll(context.Background(), pair, pushersFor(map[string]*fakePusher{"haproxy-1": one, "haproxy-2": two}), "cfg")

	require.NoError(t, PushError(outcomes))
	assert.Equal(t, []string{"cfg"}, one.pushed)
	assert.Equal(t, []string{"cfg"}, two.pushed)
}

// One instance being down is the situation the pair exists for. The survivor
// is the one serving, so it must still get the new backends.
func TestPushToAllStillUpdatesTheReachableInstanceWhenAnotherFails(t *testing.T) {
	one, two := &fakePusher{err: errors.New("dial SSH: i/o timeout")}, &fakePusher{}

	outcomes := PushToAll(context.Background(), pair, pushersFor(map[string]*fakePusher{"haproxy-1": one, "haproxy-2": two}), "cfg")

	assert.Equal(t, []string{"cfg"}, two.pushed)

	err := PushError(outcomes)
	require.Error(t, err)

	var divergent *DivergenceError
	require.ErrorAs(t, err, &divergent, "a partial push is its own kind of failure")
	assert.Equal(t, []string{"haproxy-2"}, divergent.Updated)
	assert.Equal(t, []string{"haproxy-1"}, divergent.Failed)
	assert.Contains(t, err.Error(), "haproxy-1")
	assert.Contains(t, err.Error(), "i/o timeout")
	assert.Contains(t, err.Error(), "different configs")
}

// Nothing was installed anywhere, so nothing diverged: the instances still
// agree with each other, on the old config.
func TestPushErrorIsNotADivergenceWhenEveryInstanceFailed(t *testing.T) {
	failing := &fakePusher{err: errors.New("haproxy -c: invalid config")}

	outcomes := PushToAll(context.Background(), pair, pushersFor(map[string]*fakePusher{"haproxy-1": failing, "haproxy-2": failing}), "cfg")

	err := PushError(outcomes)
	require.Error(t, err)

	var divergent *DivergenceError
	assert.NotErrorAs(t, err, &divergent)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestPushToAllReportsAnInstanceItCouldNotAuthenticateTo(t *testing.T) {
	one := &fakePusher{}

	outcomes := PushToAll(context.Background(), pair, pushersFor(map[string]*fakePusher{"haproxy-1": one}), "cfg")

	require.Len(t, outcomes, 2)
	require.NoError(t, outcomes[0].Err)
	require.Error(t, outcomes[1].Err)
	assert.Contains(t, outcomes[1].Err.Error(), "no SSH auth")
}

// A standalone failure must read exactly as it did before pairs existed.
func TestPushErrorOnAStandaloneLoadBalancerIsThePushFailureItself(t *testing.T) {
	cause := errors.New("dial SSH: connection refused")
	solo := []Instance{{Host: "192.168.1.199"}}

	outcomes := PushToAll(context.Background(), solo, func(Instance) (Pusher, error) {
		return &fakePusher{err: cause}, nil
	}, "cfg")

	err := PushError(outcomes)
	require.ErrorIs(t, err, cause)
	var divergent *DivergenceError
	assert.NotErrorAs(t, err, &divergent)
}
