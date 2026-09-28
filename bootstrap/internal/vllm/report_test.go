package vllm

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReportStatusGolden(t *testing.T) {
	res := StatusResult{
		Host: "192.168.1.50",
		Fields: []Field{
			{Name: "imageDigest", Git: "sha256:aaaa", Applied: "sha256:aaaa", Live: "docker.io/vllm/vllm-openai@sha256:bbbb"},
			{Name: "modelRevision", Git: "rev1", Applied: "rev1", Live: "n/a"},
			{Name: "servedName", Git: "qwen/q", Applied: "qwen/q", Live: "unreachable"},
			{Name: "argsHash", Git: "h1", Applied: "(none)", Live: "(not running)"},
		},
		Drift:  true,
		Legacy: true,
		Notes:  []string{"legacy vllm.service is active: the first apply replaces it with the Quadlet unit"},
		Help:   []string{"talops vllm plan  # see what apply would change and who it interrupts"},
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  drift: true
  legacy: true
fields[4]{name,git,applied,live}:
  imageDigest,sha256:aaaa,sha256:aaaa,docker.io/vllm/vllm-openai@sha256:bbbb
  modelRevision,rev1,rev1,n/a
  servedName,qwen/q,qwen/q,unreachable
  argsHash,h1,(none),(not running)
notes[1]:
  legacy vllm.service is active: the first apply replaces it with the Quadlet unit
help[1]:
  talops vllm plan  # see what apply would change and who it interrupts
`, ReportStatus(res))
}

func TestReportStatusGoldenWithFailure(t *testing.T) {
	res := StatusResult{
		Host:    "",
		Fields:  []Field{{Name: "argsHash", Git: "h1", Applied: "unknown", Live: "a,b"}},
		Drift:   true,
		Failure: &Failure{Code: CodeRead, Msg: `read applied record: exit status 1`},
	}

	assert.Equal(t, `vllm:
  host: -
  drift: true
  legacy: false
fields[1]{name,git,applied,live}:
  argsHash,h1,unknown,"a,b"
error: {code: read_failed, msg: "read applied record: exit status 1"}
`, ReportStatus(res))
}

func TestReportPlanGoldenConverged(t *testing.T) {
	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: false
diff: 0 changes — the host already runs what serving.yaml defines
`, ReportPlan(PlanResult{Host: "192.168.1.50"}))
}

func TestReportPlanGoldenChanged(t *testing.T) {
	res := PlanResult{
		Host:      "192.168.1.50",
		Changed:   true,
		Reasons:   []string{"unit /etc/containers/systemd/vllm-server.container differs from serving.yaml"},
		Diff:      "--- deployed /etc/containers/systemd/vllm-server.container\n+++ rendered\n@@ -1,1 +1,1 @@\n-Image=a\n+Image=b\n",
		Consumers: Consumers,
		Help:      []string{"talops vllm apply --confirm  # restarts the server"},
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: true
reasons[1]:
  unit /etc/containers/systemd/vllm-server.container differs from serving.yaml
diff: |
  --- deployed /etc/containers/systemd/vllm-server.container
  +++ rendered
  @@ -1,1 +1,1 @@
  -Image=a
  +Image=b
consumers[2]:
  @server answers (jdw-deployments minecraft-fwb agent.llm)
  LiteLLM sre-investigator-local (platform litellm)
help[1]:
  talops vllm apply --confirm  # restarts the server
`, ReportPlan(res))
}

// A change with no unit diff (legacy migration, a stale record) must not
// read as "nothing to do" just because the diff section is empty.
func TestReportPlanGoldenChangedWithoutUnitDiff(t *testing.T) {
	res := PlanResult{
		Host:      "192.168.1.50",
		Changed:   true,
		Reasons:   []string{"legacy vllm.service is active and is replaced by the Quadlet unit"},
		Consumers: Consumers,
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: true
reasons[1]:
  legacy vllm.service is active and is replaced by the Quadlet unit
diff: 0 lines — the unit already matches; apply acts for the reasons above
consumers[2]:
  @server answers (jdw-deployments minecraft-fwb agent.llm)
  LiteLLM sre-investigator-local (platform litellm)
`, ReportPlan(res))
}

func TestReportPlanGoldenFailure(t *testing.T) {
	res := PlanResult{Host: "192.168.1.50", Failure: &Failure{Code: CodeRead, Msg: "read installed unit: boom"}}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: false
error: {code: read_failed, msg: "read installed unit: boom"}
`, ReportPlan(res))
}

func TestReportApplyGoldenSuccess(t *testing.T) {
	res := ApplyResult{
		Host:    "192.168.1.50",
		Changed: true,
		Serving: ServingNew,
		Reasons: []string{"unit /etc/containers/systemd/vllm-server.container differs from serving.yaml"},
		Phases: []Phase{
			{Name: "host", Took: 1200 * time.Millisecond},
			{Name: "stage", Took: 3*time.Minute + 4*time.Second},
			{Name: "converge", Took: 2 * time.Second},
			{Name: "gate", Took: 95*time.Second + 123456*time.Microsecond},
			{Name: "record", Took: 300 * time.Millisecond},
		},
		Notes: []string{"host: warning: prometheus-node-exporter's --collector.textfile.directory does not match"},
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: true
  rolledBack: false
  serving: new
reasons[1]:
  unit /etc/containers/systemd/vllm-server.container differs from serving.yaml
phases[5]{name,took}:
  host,1.2s
  stage,3m4s
  converge,2s
  gate,1m35.123s
  record,300ms
notes[1]:
  host: warning: prometheus-node-exporter's --collector.textfile.directory does not match
`, ReportApply(res))
}

func TestReportApplyGoldenNoOp(t *testing.T) {
	res := ApplyResult{
		Host:    "192.168.1.50",
		Serving: ServingNew,
		Phases:  []Phase{{Name: "host", Took: time.Second}},
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: false
  rolledBack: false
  serving: new
phases[1]{name,took}:
  host,1s
result: 0 changes — the host already runs what serving.yaml defines; the server was not restarted
`, ReportApply(res))
}

func TestReportApplyGoldenRolledBackButDown(t *testing.T) {
	res := ApplyResult{
		Host:       "192.168.1.50",
		Changed:    true,
		RolledBack: true,
		Serving:    ServingNone,
		Phases:     []Phase{{Name: "gate", Took: 10 * time.Minute}, {Name: "gate-rollback", Took: 10 * time.Minute}},
		Failure:    &Failure{Code: CodeGate, Msg: `models check: no entry with id "q"; rollback restored the previous config, which is unhealthy too: timed out`},
		Help: []string{
			"the vLLM endpoint is down: rollback restored the previous files but that server is not healthy either",
			"recover by hand with scenarios/ai-sre-agent-runbook.md",
		},
	}

	assert.Equal(t, `vllm:
  host: 192.168.1.50
  changed: true
  rolledBack: true
  serving: none
phases[2]{name,took}:
  gate,10m0s
  gate-rollback,10m0s
error: {code: gate_failed, msg: "models check: no entry with id \"q\"; rollback restored the previous config, which is unhealthy too: timed out"}
help[2]:
  the vLLM endpoint is down: rollback restored the previous files but that server is not healthy either
  recover by hand with scenarios/ai-sre-agent-runbook.md
`, ReportApply(res))
}

// A multi-line failure (errors.Join from the health gate) must stay on one
// line: the error block is a single TOON value.
func TestReportKeepsMultiLineErrorsOnOneLine(t *testing.T) {
	out := ReportApply(ApplyResult{Failure: &Failure{Code: CodeGate, Msg: "first\nsecond"}})
	assert.Contains(t, out, `error: {code: gate_failed, msg: "first\nsecond"}`)
	assert.Equal(t, 1, strings.Count(out, "error:"))
}

func TestLineDiff(t *testing.T) {
	from := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"
	to := "a\nb\nc\nD\ne\nf\ng\nh\ni\nj\nk\nl\n"

	assert.Equal(t, `--- old
+++ new
@@ -1,7 +1,7 @@
 a
 b
 c
-d
+D
 e
 f
 g
@@ -9,3 +9,4 @@
 i
 j
 k
+l
`, lineDiff(from, to, "old", "new"))
}

func TestLineDiffEquivalentIsEmpty(t *testing.T) {
	assert.Empty(t, lineDiff("a\nb\n", "a\r\nb", "old", "new"), "a line-ending round trip is not a change")
}

func TestLineDiffFromNothing(t *testing.T) {
	assert.Equal(t, "--- old\n+++ new\n@@ -0,0 +1,2 @@\n+a\n+b\n", lineDiff("", "a\nb\n", "old", "new"))
}

// A list item is a TOON value like any other; one with a comma or a quote
// in it must not read as two values.
func TestReportQuotesListItemsThatWouldBreakTheirLine(t *testing.T) {
	out := ReportApply(ApplyResult{
		Serving: ServingNew,
		Changed: true,
		Reasons: []string{"/etc/vllm/applied.json records a different image, model revision or args"},
		Notes:   []string{`host: warning: "quoted"`},
		Help:    []string{"plain help"},
	})
	assert.Contains(t, out, "reasons[1]:\n  \"/etc/vllm/applied.json records a different image, model revision or args\"\n")
	assert.Contains(t, out, "notes[1]:\n  \"host: warning: \\\"quoted\\\"\"\n")
	assert.Contains(t, out, "help[1]:\n  plain help\n")
}
