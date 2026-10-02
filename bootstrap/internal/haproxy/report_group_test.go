package haproxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func member(name, host, holds string) InstanceStatus {
	return InstanceStatus{
		Instance:     Instance{Name: name, Host: host},
		VM:           VMInfo{Name: name, State: "running", Source: "terraform"},
		SSH:          "ok",
		Service:      "active",
		Keepalived:   "active",
		HoldsAddress: holds,
		ConfigDrift:  "false",
	}
}

func groupStatus(t *testing.T, instances ...InstanceStatus) GroupStatusResult {
	t.Helper()
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)
	for i := range instances {
		if instances[i].SSH == "ok" {
			instances[i].Backends = stats
			instances[i].BackendsUp = UpCount(stats)
			instances[i].BackendsTotal = len(stats)
		}
	}
	res := GroupStatusResult{VirtualAddress: "192.168.1.199", Instances: instances}
	res.Assess()
	return res
}

func TestAssessNamesTheInstanceHoldingTheVirtualAddress(t *testing.T) {
	res := groupStatus(t, member("haproxy-1", "192.168.1.11", "false"), member("haproxy-2", "192.168.1.12", "true"))

	assert.Equal(t, "haproxy-2", res.Holder)
	assert.Nil(t, res.Failure)
	assert.Equal(t, "haproxy-2", res.BackendsFrom, "the holder's view is the one clients are served from")
}

// Two holders is the worst state a pair can be in: both answer ARP, and each
// client's connections land on whichever instance its neighbour table names.
func TestAssessReportsTwoHoldersAsASplit(t *testing.T) {
	res := groupStatus(t, member("haproxy-1", "192.168.1.11", "true"), member("haproxy-2", "192.168.1.12", "true"))

	assert.Equal(t, HolderSplit, res.Holder)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "virtual_address_split", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-1 and haproxy-2")
}

func TestAssessReportsAnUnheldAddressWhenEveryInstanceWasAsked(t *testing.T) {
	res := groupStatus(t, member("haproxy-1", "192.168.1.11", "false"), member("haproxy-2", "192.168.1.12", "false"))

	assert.Equal(t, HolderNone, res.Holder)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "virtual_address_unheld", res.Failure.Code)
}

// The endpoint is up, and the report must still fail: with one member gone
// there is nothing left to fail over to.
func TestAssessFailsADegradedGroupEvenWhileTheAddressIsHeld(t *testing.T) {
	down := member("haproxy-1", "192.168.1.11", Unknown)
	down.SSH, down.Service, down.Keepalived, down.ConfigDrift = "unreachable", Unknown, Unknown, Unknown

	res := groupStatus(t, down, member("haproxy-2", "192.168.1.12", "true"))

	assert.Equal(t, "haproxy-2", res.Holder)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "instance_unreachable", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-1")
}

// With the only reachable instance not holding it, "none" would claim the
// endpoint is dark when the unreachable one may well be serving.
func TestAssessDoesNotClaimTheAddressIsUnheldWhenAnInstanceWasNotAsked(t *testing.T) {
	down := member("haproxy-1", "192.168.1.11", Unknown)
	down.SSH = "unreachable"

	res := groupStatus(t, down, member("haproxy-2", "192.168.1.12", "false"))

	assert.Equal(t, Unknown, res.Holder)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "instance_unreachable", res.Failure.Code)
	assert.Equal(t, "haproxy-2", res.BackendsFrom, "without a holder, the first readable table is shown")
}

func TestAssessFailsWhenAMemberCannotTakeOver(t *testing.T) {
	idle := member("haproxy-1", "192.168.1.11", "false")
	idle.Keepalived = "inactive"

	res := groupStatus(t, idle, member("haproxy-2", "192.168.1.12", "true"))

	require.NotNil(t, res.Failure)
	assert.Equal(t, "keepalived_inactive", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-1")
}

func TestReportGroupStatusPrintsOneRowPerInstanceAndTheHolder(t *testing.T) {
	out := ReportGroupStatus(groupStatus(t,
		member("haproxy-1", "192.168.1.11", "false"), member("haproxy-2", "192.168.1.12", "true")))

	assert.Contains(t, out, "  virtualAddress: 192.168.1.199\n  holder: haproxy-2\n")
	assert.Contains(t, out, "instances[2]{name,host,vm,ssh,service,keepalived,holdsAddress,configDrift,backendsUp}:")
	assert.Contains(t, out, "  haproxy-1,192.168.1.11,running,ok,active,active,false,false,2/3\n")
	assert.Contains(t, out, "  haproxy-2,192.168.1.12,running,ok,active,active,true,false,2/3\n")
	assert.Contains(t, out, "backendsFrom: haproxy-2\nbackends[3]{name,addr,status,check}:")
	assert.Contains(t, out, "backendsUp: 2/3")
}

func TestReportGroupStatusShowsWhyAVMLayerWasNotRead(t *testing.T) {
	undeployed := member("haproxy-2", "192.168.1.12", Unknown)
	undeployed.VM = VMInfo{Name: "haproxy-2", State: Unknown, Source: "declared"}
	undeployed.SSH = "unreachable"

	out := ReportGroupStatus(groupStatus(t, member("haproxy-1", "192.168.1.11", "true"), undeployed))

	assert.Contains(t, out, "  haproxy-2,192.168.1.12,declared,unreachable,")
	assert.Contains(t, out, "error: {code: instance_unreachable")
}

func TestGroupStatusJSONCarriesEveryInstance(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EmitJSON(&buf, "status", groupStatus(t,
		member("haproxy-1", "192.168.1.11", "false"), member("haproxy-2", "192.168.1.12", "true"))))

	var got struct {
		Event     string `json:"event"`
		Holder    string `json:"holder"`
		Instances []struct {
			Name                string `json:"name"`
			Host                string `json:"host"`
			HoldsVirtualAddress string `json:"holdsVirtualAddress"`
		} `json:"instances"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "status", got.Event)
	assert.Equal(t, "haproxy-2", got.Holder)
	require.Len(t, got.Instances, 2)
	assert.Equal(t, "192.168.1.12", got.Instances[1].Host)
	assert.Equal(t, "true", got.Instances[1].HoldsVirtualAddress)
}

func TestReportGroupPlanSharesOneDiffBetweenInstancesThatDriftAlike(t *testing.T) {
	diff := "-    server talos-cp-201 192.168.1.21:6443 check\n+    server talos-cp-201 192.168.1.41:6443 check\n"
	out := ReportGroupPlan(GroupPlanResult{
		VirtualAddress: "192.168.1.199",
		Drift:          true,
		Backends:       3,
		RenderedHash:   "aaaaaaaaaaaaaaaa",
		Instances: []InstancePlan{
			{Instance: Instance{Name: "haproxy-1", Host: "192.168.1.11"}, Drift: "true", DeployedHash: "bbbbbbbbbbbbbbbb", Diff: diff},
			{Instance: Instance{Name: "haproxy-2", Host: "192.168.1.12"}, Drift: "true", DeployedHash: "bbbbbbbbbbbbbbbb", Diff: diff},
		},
	})

	assert.Contains(t, out, "instances[2]{name,host,drift,deployedHash}:\n  haproxy-1,192.168.1.11,true,bbbbbbbbbbbb\n")
	assert.Contains(t, out, "diffs:\n  haproxy-1: |\n    -    server talos-cp-201 192.168.1.21:6443 check\n")
	assert.Contains(t, out, "  haproxy-2: same as haproxy-1\n")
}

// One member behind the other is the divergence a plan exists to show: the
// drifted one gets a diff, the converged one must not be drawn as drifting.
func TestReportGroupPlanShowsOnlyTheDriftingInstance(t *testing.T) {
	out := ReportGroupPlan(GroupPlanResult{
		VirtualAddress: "192.168.1.199",
		Drift:          true,
		Instances: []InstancePlan{
			{Instance: Instance{Name: "haproxy-1", Host: "192.168.1.11"}, Drift: "false"},
			{Instance: Instance{Name: "haproxy-2", Host: "192.168.1.12"}, Drift: "true", Diff: "+new\n", DiffBytes: 5000, Truncated: true},
		},
	})

	assert.Contains(t, out, "  haproxy-1,192.168.1.11,false,")
	assert.Contains(t, out, "  haproxy-2: |\n    +new\n    ... (truncated, 5000 chars total — use --full)\n")
	assert.NotContains(t, out, "  haproxy-1: |")
}

func TestReportGroupPlanStatesConvergenceExplicitly(t *testing.T) {
	out := ReportGroupPlan(GroupPlanResult{
		VirtualAddress: "192.168.1.199",
		Instances: []InstancePlan{
			{Instance: Instance{Name: "haproxy-1", Host: "192.168.1.11"}, Drift: "false"},
			{Instance: Instance{Name: "haproxy-2", Host: "192.168.1.12"}, Drift: "false"},
		},
	})

	assert.Contains(t, out, "diff: 0 changes — every instance already matches current cluster state")
}

func applied(name, host string, drift, changed bool, err string) InstanceApply {
	return InstanceApply{Instance: Instance{Name: name, Host: host}, Drift: drift, Changed: changed, Error: err}
}

// The failure a pair adds over a standalone load balancer: the push "worked",
// and the instances now disagree.
func TestConcludeNamesAPartialPushAsDivergence(t *testing.T) {
	res := GroupApplyResult{VirtualAddress: "192.168.1.199", Instances: []InstanceApply{
		applied("haproxy-1", "192.168.1.11", true, true, ""),
		applied("haproxy-2", "192.168.1.12", true, false, "dial SSH: i/o timeout"),
	}}
	res.Conclude()

	assert.True(t, res.Changed)
	assert.True(t, res.Drift)
	require.NotNil(t, res.Failure)
	assert.Equal(t, "group_divergent", res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "haproxy-2")

	out := ReportGroupApply(res)
	assert.Contains(t, out, "instances[2]{name,host,drift,changed,error}:")
	assert.Contains(t, out, "  haproxy-1,192.168.1.11,true,true,-\n")
	assert.Contains(t, out, "  haproxy-2,192.168.1.12,true,false,dial SSH: i/o timeout\n")
	assert.Contains(t, out, "error: {code: group_divergent")
	assert.NotContains(t, out, "result: 0 changes")
}

// Nothing was pushed anywhere, and the instances still disagree: one was
// already current and the other could not be brought up to it.
func TestConcludeFailsWhenOneInstanceCouldNotBePushedAndTheOtherNeededNothing(t *testing.T) {
	res := GroupApplyResult{Instances: []InstanceApply{
		applied("haproxy-1", "192.168.1.11", false, false, ""),
		applied("haproxy-2", "192.168.1.12", true, false, "haproxy -c failed"),
	}}
	res.Conclude()

	require.NotNil(t, res.Failure)
	assert.Equal(t, "group_divergent", res.Failure.Code)
}

func TestConcludeReportsAPushThatFailedEverywhereAsPlainFailure(t *testing.T) {
	res := GroupApplyResult{Instances: []InstanceApply{
		applied("haproxy-1", "192.168.1.11", true, false, "haproxy -c failed"),
		applied("haproxy-2", "192.168.1.12", true, false, "haproxy -c failed"),
	}}
	res.Conclude()

	require.NotNil(t, res.Failure)
	assert.Equal(t, "push_failed", res.Failure.Code)
	assert.False(t, res.Changed)
}

func TestReportGroupApplyStatesTheNoOpExplicitly(t *testing.T) {
	res := GroupApplyResult{VirtualAddress: "192.168.1.199", Instances: []InstanceApply{
		applied("haproxy-1", "192.168.1.11", false, false, ""),
		applied("haproxy-2", "192.168.1.12", false, false, ""),
	}}
	res.Conclude()

	require.Nil(t, res.Failure)
	assert.Contains(t, ReportGroupApply(res), "result: 0 changes pushed — every instance was already current")
}

// A dry run with drift pushed nothing, but saying the instances were "already
// current" would be the opposite of what it found.
func TestReportGroupApplyDryRunWithDriftDoesNotClaimConvergence(t *testing.T) {
	res := GroupApplyResult{DryRun: true, Instances: []InstanceApply{
		applied("haproxy-1", "192.168.1.11", true, false, ""),
		applied("haproxy-2", "192.168.1.12", false, false, ""),
	}}
	res.Conclude()

	out := ReportGroupApply(res)
	assert.Contains(t, out, "  dryRun: true")
	assert.NotContains(t, out, "already current")
}
