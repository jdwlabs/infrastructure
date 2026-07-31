package haproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func statusFixture(t *testing.T) StatusResult {
	t.Helper()
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	return StatusResult{
		Host:        "192.168.1.199",
		VM:          VMInfo{Name: "haproxy-1", VMID: 110, Node: "pve1", State: "running", Source: "terraform"},
		SSH:         "ok",
		Service:     "active",
		ConfigDrift: "false",
		Backends:    stats,
		Fields:      DefaultStatFields,
	}
}

func TestReportStatusUsesTheDefaultFourColumnSchema(t *testing.T) {
	out := ReportStatus(statusFixture(t))

	assert.Contains(t, out, "backends[3]{name,addr,status,check}:")
	assert.Contains(t, out, "  talos-cp-201,192.168.1.21:6443,UP,L4OK")
	assert.Contains(t, out, "vm: {name: haproxy-1, vmid: 110, node: pve1, state: running, source: terraform}")
}

// The aggregate is what the caller needs next; without it they re-read the
// table to count it themselves.
func TestReportStatusPrecomputesTheUpAggregate(t *testing.T) {
	assert.Contains(t, ReportStatus(statusFixture(t)), "backendsUp: 2/3")
}

func TestReportStatusHonoursRequestedFields(t *testing.T) {
	res := statusFixture(t)
	res.Fields = []string{"name", "downtime"}

	out := ReportStatus(res)
	assert.Contains(t, out, "backends[3]{name,downtime}:")
	assert.Contains(t, out, "  talos-cp-202,182")
	assert.NotContains(t, out, "192.168.1.21:6443")
}

// An empty section reads as "the check did not run" and invites a second call
// with different flags.
func TestReportStatusStatesTheZeroExplicitly(t *testing.T) {
	res := statusFixture(t)
	res.Backends = nil

	out := ReportStatus(res)
	assert.Contains(t, out, "backends: 0 server rows reported by HAProxy")
	assert.NotContains(t, out, "backendsUp:")
}

func TestReportStatusCarriesNotesAndStructuredErrors(t *testing.T) {
	res := statusFixture(t)
	res.ConfigDrift = Unknown
	res.Notes = []string{"config drift unchecked: read /etc/haproxy/haproxy.cfg: permission denied"}
	res.Failure = &Failure{Code: "ssh_unreachable", Msg: "SSH to root@192.168.1.199 failed"}
	res.Help = []string{"talops haproxy plan  # review the config change"}

	out := ReportStatus(res)
	assert.Contains(t, out, "configDrift: unknown")
	assert.Contains(t, out, "notes[1]:")
	assert.Contains(t, out, "error: {code: ssh_unreachable,")
	assert.Contains(t, out, "help[1]:")
}

func TestReportPlanStatesConvergenceRatherThanPrintingAnEmptyDiff(t *testing.T) {
	out := ReportPlan(PlanResult{Host: "192.168.1.199", Drift: false, Backends: 8, RenderedHash: strings.Repeat("a", 64)})

	assert.Contains(t, out, "drift: false")
	assert.Contains(t, out, "diff: 0 changes — the deployed config already matches current cluster state")
}

func TestReportPlanIndentsTheDiffAndFlagsTruncation(t *testing.T) {
	out := ReportPlan(PlanResult{
		Host:      "192.168.1.199",
		Drift:     true,
		Diff:      "--- deployed\n+++ rendered\n-old\n+new\n",
		DiffBytes: 2841,
		Truncated: true,
	})

	assert.Contains(t, out, "diff: |\n")
	assert.Contains(t, out, "  -old")
	assert.Contains(t, out, "... (truncated, 2841 chars total — use --full)")
}

func TestReportApplyNamesTheNoOp(t *testing.T) {
	out := ReportApply(ApplyResult{Host: "192.168.1.199", Changed: false, Drift: false, Backends: 8})

	assert.Contains(t, out, "changed: false")
	assert.Contains(t, out, "result: 0 changes pushed — the deployed config was already current")
}

func TestReportApplyOmitsTheNoOpLineWhenSomethingChanged(t *testing.T) {
	out := ReportApply(ApplyResult{Host: "192.168.1.199", Changed: true, Drift: true, Backends: 8})

	assert.Contains(t, out, "changed: true")
	assert.NotContains(t, out, "0 changes pushed")
}

// A comma inside a cell would silently shift every column after it.
func TestToonCellQuotesValuesThatWouldBreakTheRow(t *testing.T) {
	assert.Equal(t, `"a,b"`, toonCell("a,b"))
	assert.Equal(t, "-", toonCell(""))
	assert.Equal(t, "UP", toonCell("UP"))
}

func TestEmitJSONWritesOneObjectPerLineTaggedWithTheTransition(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EmitJSON(&buf, "rendered", ApplyResult{Host: "192.168.1.199", Backends: 8}))
	require.NoError(t, EmitJSON(&buf, "apply", ApplyResult{Host: "192.168.1.199", Backends: 8, Changed: true}))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)

	var first, second map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))

	assert.Equal(t, "rendered", first["event"])
	assert.Equal(t, "apply", second["event"])
	assert.Equal(t, true, second["changed"])
	assert.Equal(t, "192.168.1.199", second["host"])
}

func TestBackendsJSONProjectsOnlyTheSelectedColumns(t *testing.T) {
	stats, err := ParseStats(sampleStatsCSV)
	require.NoError(t, err)

	rows := BackendsJSON(stats, []string{"name", "status"})
	require.Len(t, rows, 3)
	assert.Equal(t, map[string]string{"name": "talos-cp-201", "status": "UP"}, rows[0])
}

func TestReportErrorPairsTheFailureWithItsFix(t *testing.T) {
	out := ReportError(&Failure{Code: "unknown_flag", Msg: "unknown flag: --stat"},
		[]string{"valid flags for `talops haproxy status`: --host, --fields"})

	assert.Contains(t, out, "error: {code: unknown_flag,")
	assert.Contains(t, out, "help[1]:")
	assert.Contains(t, out, "--host, --fields")
}
