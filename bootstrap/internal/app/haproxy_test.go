package app

import (
	"net"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/terraform"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func haproxyTestContext() *haproxyContext {
	cfg := types.DefaultConfig()
	cfg.HAProxyIP = net.ParseIP("192.168.1.199")
	cfg.IngressHTTPNodePort = 30080
	cfg.IngressTLSNodePort = 30443

	return &haproxyContext{
		cfg:  cfg,
		host: "192.168.1.199",
		deployed: &types.ClusterState{
			ControlPlanes: []types.NodeState{
				{VMID: 201, IP: net.ParseIP("192.168.1.21")},
				{VMID: 202, IP: net.ParseIP("192.168.1.22")},
			},
			Workers: []types.NodeState{
				{VMID: 301, IP: net.ParseIP("192.168.1.31")},
			},
		},
	}
}

func TestRenderConfigCountsEveryBackend(t *testing.T) {
	rendered, backends, failure := haproxyTestContext().renderConfig()

	require.Nil(t, failure)
	assert.Equal(t, 3, backends, "two control planes plus one ingress node")
	assert.Contains(t, rendered, "server talos-cp-201 192.168.1.21:6443 check")
	assert.Contains(t, rendered, "server ingress-301 192.168.1.31:30080 check")
}

// The generator binds the frontends to the address it is handed. Rendering for
// an override host without carrying it through would produce a config that
// binds the production address on a VM that does not hold it.
func TestRenderConfigBindsTheOverriddenHost(t *testing.T) {
	hc := haproxyTestContext()
	hc.host = "192.168.1.198"

	rendered, _, failure := hc.renderConfig()

	require.Nil(t, failure)
	assert.Contains(t, rendered, "bind 192.168.1.198:6443")
	assert.NotContains(t, rendered, "bind 192.168.1.199:6443")
	assert.Equal(t, "192.168.1.199", hc.cfg.HAProxyIP.String(), "the override must not leak into shared config")
}

func TestRenderConfigRefusesWithoutControlPlanes(t *testing.T) {
	hc := haproxyTestContext()
	hc.deployed.ControlPlanes = nil

	_, _, failure := hc.renderConfig()

	require.NotNil(t, failure)
	assert.Equal(t, "no_control_planes", failure.Code)
	assert.NotEmpty(t, helpForFailure(failure), "a refusal must name the command that fixes it")
}

func stateWith(resources ...terraform.StateResource) *terraform.StateOutput {
	return &terraform.StateOutput{
		Values: &terraform.StateValues{RootModule: &terraform.RootModule{Resources: resources}},
	}
}

func TestFindHAProxyVMStateReadsTheRunningFlag(t *testing.T) {
	out := stateWith(terraform.StateResource{
		Type:   "proxmox_virtual_environment_vm",
		Name:   "haproxy",
		Values: map[string]any{"vm_id": float64(110), "started": true},
	})

	assert.Equal(t, "running", findHAProxyVMState(out, 110))
}

func TestFindHAProxyVMStateReportsAStoppedVM(t *testing.T) {
	out := stateWith(terraform.StateResource{
		Type:   "proxmox_virtual_environment_vm",
		Name:   "haproxy",
		Values: map[string]any{"vm_id": float64(110), "started": false},
	})

	assert.Equal(t, "stopped", findHAProxyVMState(out, 110))
}

// The control-plane and worker VMs are the same resource type. Matching on type
// alone would report a control plane's state as the load balancer's.
func TestFindHAProxyVMStateIgnoresOtherVMResources(t *testing.T) {
	out := stateWith(
		terraform.StateResource{
			Type:   "proxmox_virtual_environment_vm",
			Name:   "controlplane",
			Values: map[string]any{"vm_id": float64(110), "started": true},
		},
		terraform.StateResource{
			Type:   "proxmox_virtual_environment_vm",
			Name:   "haproxy",
			Values: map[string]any{"vm_id": float64(111), "started": true},
		},
	)

	assert.Empty(t, findHAProxyVMState(out, 110), "VMID 110 belongs to a control plane here")
	assert.Equal(t, "running", findHAProxyVMState(out, 111))
}

func TestFindHAProxyVMStateHandlesEmptyState(t *testing.T) {
	assert.Empty(t, findHAProxyVMState(nil, 110))
	assert.Empty(t, findHAProxyVMState(&terraform.StateOutput{}, 110))
	assert.Empty(t, findHAProxyVMState(stateWith(), 110))
}

func TestResolveFieldsFullExpandsToEveryColumn(t *testing.T) {
	stats, err := haproxy.ParseStats("# svname,status,addr,check_status,downtime,\ntalos-cp-201,UP,192.168.1.21:6443,L4OK,0,\n")
	require.NoError(t, err)

	fields, failure := resolveFields(HAProxyOptions{Full: true}, stats)
	require.Nil(t, failure)
	assert.Contains(t, fields, "downtime")
	assert.Greater(t, len(fields), len(haproxy.DefaultStatFields))
}

func TestResolveFieldsRejectsAnUnknownColumn(t *testing.T) {
	stats, err := haproxy.ParseStats("# svname,status,\ntalos-cp-201,UP,\n")
	require.NoError(t, err)

	_, failure := resolveFields(HAProxyOptions{Fields: []string{"helth"}}, stats)
	require.NotNil(t, failure)
	assert.Equal(t, "unknown_field", failure.Code)
	assert.NotEmpty(t, helpForFailure(failure))
}

// The hand-built load balancer is the case this distinction exists for: it is
// reachable and healthy, and still not reproducible from this repo.
func TestVMHelpPointsAnUnmanagedLoadBalancerAtTheRebuildRunbook(t *testing.T) {
	help := vmHelp(haproxy.VMInfo{Source: "unmanaged"})
	require.Len(t, help, 1)
	assert.Contains(t, help[0], "scenarios/haproxy-vm-rebuild.md")
}

func TestVMHelpPointsADeclaredButUnappliedVMAtTheHumanGate(t *testing.T) {
	help := vmHelp(haproxy.VMInfo{Source: "declared"})
	require.Len(t, help, 1)
	assert.Contains(t, help[0], "talops infra plan")
	assert.Contains(t, help[0], "a human applies it")
}

func TestVMHelpIsSilentForATerraformManagedVM(t *testing.T) {
	assert.Empty(t, vmHelp(haproxy.VMInfo{Source: "terraform"}))
}

func TestStatusHelpSuggestsPlanOnlyWhenTheConfigIsNotKnownGood(t *testing.T) {
	drifted := statusHelp(haproxy.StatusResult{ConfigDrift: "true"}, haproxy.VMInfo{Source: "terraform"})
	require.NotEmpty(t, drifted)
	assert.Contains(t, drifted[0], "talops haproxy plan")

	converged := statusHelp(haproxy.StatusResult{ConfigDrift: "false"}, haproxy.VMInfo{Source: "terraform"})
	assert.Empty(t, converged, "a self-contained answer needs no next step")
}

// A backend that is down is a node problem, not a load-balancer problem, and
// the suggestion has to send the caller to the right layer.
func TestStatusHelpPointsADownBackendAtTheNode(t *testing.T) {
	stats, err := haproxy.ParseStats("# svname,status,\ntalos-cp-201,UP,\ntalos-cp-202,DOWN,\n")
	require.NoError(t, err)

	help := statusHelp(
		haproxy.StatusResult{ConfigDrift: "false", Backends: stats},
		haproxy.VMInfo{Source: "terraform"},
	)
	require.Len(t, help, 1)
	assert.Contains(t, help[0], "check the node itself")
}

func TestPlanHelpOffersTheFullDiffOnlyWhenItWasTruncated(t *testing.T) {
	truncated := planHelp(haproxy.PlanResult{Drift: true, Truncated: true, DiffBytes: 2841})
	require.Len(t, truncated, 2)
	assert.Contains(t, truncated[0], "talops haproxy apply")
	assert.Contains(t, truncated[1], "--full")

	whole := planHelp(haproxy.PlanResult{Drift: true})
	require.Len(t, whole, 1)
	assert.NotContains(t, whole[0], "--full")
}

func TestPlanHelpOnAConvergedConfigPointsAtBackendHealth(t *testing.T) {
	help := planHelp(haproxy.PlanResult{Drift: false})
	require.Len(t, help, 1)
	assert.Contains(t, help[0], "talops haproxy status")
}

func TestApplyHelpDistinguishesADryRunFromANoOp(t *testing.T) {
	dry := applyHelp(haproxy.ApplyResult{DryRun: true, Drift: true})
	require.Len(t, dry, 1)
	assert.Contains(t, dry[0], "without --dry-run")

	assert.Empty(t, applyHelp(haproxy.ApplyResult{Changed: false, Drift: false}))

	changed := applyHelp(haproxy.ApplyResult{Changed: true, Drift: true})
	require.Len(t, changed, 1)
	assert.Contains(t, changed[0], "talops haproxy status")
}
