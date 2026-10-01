package modelaudit

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenReport exercises every section: one candidate with a gated repo and
// extra args, a reason containing a comma, a skipped source and a failure.
func goldenReport() Report {
	return Report{
		Summary: Summary{
			Week: "2026-W40", Model: "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ@c58857a7f41c0920f73d1b56678640f9c02017d7",
			ServedName: "qwen/qwen3-coder-30b-a3b", VLLMTag: "v0.24.0", BudgetGiB: 28.66,
			Checked: 3, Candidates: 1, Rejected: 2, Requests: 9, DryRun: true,
		},
		Candidates: []CandidateRow{{
			Repo: "Qwen/Qwen3-Coder-Next-Instruct", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Pool: PoolAllowListed,
			CreatedAt: time.Date(2026, 9, 26, 6, 0, 0, 0, time.UTC), Downloads: 500, Quant: "awq",
			WeightsGiB: 15.66, TotalGiB: 21.66, MarginGiB: 7.00, Parser: "qwen3_xml", License: "apache-2.0", Gated: "manual",
			URL: "https://huggingface.co/Qwen/Qwen3-Coder-Next-Instruct/tree/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			TrialSteps: []string{
				"set --tool-call-parser=qwen3_xml",
				"add --chat-template=<model's tool chat template>",
			},
		}},
		Reasons: []ReasonCount{{Reason: "license other (a, b) not allowed", Count: 1}, {Reason: "pipeline_tag missing", Count: 1}},
		Rejected: []Rejection{
			{Repo: "Qwen/Image", Reason: "pipeline_tag missing"},
			{Repo: "Org/Odd", Reason: "license other (a, b) not allowed"},
		},
		Skipped: []Skip{{Source: "trending", Error: "not read: request budget spent"}},
		Jira:    JiraOutcome{Action: "not-filed"},
		Failure: &Failure{Code: "example", Msg: "two\nlines"},
	}
}

func TestRenderTOONMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/report.toon.golden")
	require.NoError(t, err)
	assert.Equal(t, string(want), RenderTOON(goldenReport()))
}

func TestRenderJSONMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/report.json.golden")
	require.NoError(t, err)
	got, err := RenderJSON(goldenReport())
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
	assert.Equal(t, string(want), string(got), "key order is part of the contract")
}

func TestRenderJSONEmptyReportUsesEmptyListsNotNull(t *testing.T) {
	got, err := RenderJSON(Report{Summary: Summary{Week: "2026-W40"}, Jira: JiraOutcome{Action: "none"}})
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &m))
	for _, k := range []string{"candidates", "rejectionCounts", "rejected", "skipped"} {
		assert.Equal(t, "[]", string(m[k]), k)
	}
	assert.NotContains(t, m, "error")
}

func TestRenderTOONEmptyReportStatesEverySection(t *testing.T) {
	out := RenderTOON(Report{Summary: Summary{Week: "2026-W40"}, Jira: JiraOutcome{Action: "none"}})
	for _, h := range []string{"candidates[0]{", "rejectionCounts[0]{", "rejected[0]{", "skipped[0]{", "jira:\n  action: none\n  key: -\n"} {
		assert.Contains(t, out, h)
	}
	assert.NotContains(t, out, "trialSteps")
}
