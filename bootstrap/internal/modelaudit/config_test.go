package modelaudit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfig = `
windowDays: 7
trendingWindowDays: 30
trendingN: 50
maxCandidates: 10
maxRequests: 400
discoveryRequests: 120
gpuMemMiB: 32607
overheadGiB: 3
orgs: [Qwen, QuantTrio]
licenses: {allow: [apache-2.0], allowNames: []}
parsers:
  - {modelType: qwen3_moe, nameRegex: "(?i)coder", parser: qwen3_xml}
  - {modelType: qwen3_moe, parser: hermes}
  - {modelType: mistral, parser: mistral, extraArgs: ["--tokenizer-mode=mistral"]}
jira: {project: AUDIT, parent: AUDIT-1, issueType: Task}
`

func TestParseConfigAcceptsAValidConfig(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	require.NoError(t, err)
	assert.Equal(t, 7, c.WindowDays)
	assert.Equal(t, []string{"Qwen", "QuantTrio"}, c.Orgs)
	assert.Len(t, c.Parsers, 3)
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string][2]string{
		"unknown key":                  {"windowDays: 7", "windowDays: 7\ncontextTokens: 32768"},
		"window out of range":          {"windowDays: 7", "windowDays: 0"},
		"trendingN out of range":       {"trendingN: 50", "trendingN: 101"},
		"maxRequests over the Hub cap": {"maxRequests: 400", "maxRequests: 501"},
		"discovery not below max":      {"discoveryRequests: 120", "discoveryRequests: 400"},
		"negative overhead":            {"overheadGiB: 3", "overheadGiB: -1"},
		"no orgs":                      {"orgs: [Qwen, QuantTrio]", "orgs: []"},
		"duplicate org":                {"orgs: [Qwen, QuantTrio]", "orgs: [Qwen, Qwen]"},
		"bad regex":                    {`nameRegex: "(?i)coder"`, `nameRegex: "(?i)coder("`},
		"rule without parser":          {"{modelType: qwen3_moe, parser: hermes}", "{modelType: qwen3_moe}"},
		"lowercase project":            {"project: AUDIT,", "project: audit,"},
		"parent outside project":       {"parent: AUDIT-1", "parent: OTHER-1"},
		"parent not an issue key":      {"parent: AUDIT-1", "parent: AUDIT"},
		"no issue type":                {"issueType: Task", `issueType: ""`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validConfig, c[0], c[1], 1)
			require.NotEqual(t, validConfig, mutated, "the mutation must apply")
			_, err := ParseConfig([]byte(mutated))
			assert.Error(t, err)
		})
	}
}

func TestParseConfigRejectsNoRules(t *testing.T) {
	cut := validConfig[:strings.Index(validConfig, "parsers:")] + "parsers: []\njira: {project: AUDIT, parent: AUDIT-1, issueType: Task}\n"
	_, err := ParseConfig([]byte(cut))
	assert.ErrorContains(t, err, "at least one rule")
}

func TestResolveParserFirstMatchWinsAndNameRegexGates(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	require.NoError(t, err)

	r, ok := c.ResolveParser("Qwen/Qwen3-Coder-30B-A3B-Instruct", "qwen3_moe", "")
	require.True(t, ok)
	assert.Equal(t, "qwen3_xml", r.Parser)

	r, ok = c.ResolveParser("Qwen/Qwen3-30B-A3B-Instruct-2507", "qwen3_moe", "")
	require.True(t, ok)
	assert.Equal(t, "hermes", r.Parser)
}

func TestResolveParserFallsBackToTextConfigModelType(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	require.NoError(t, err)

	r, ok := c.ResolveParser("mistralai/Mistral-Small-3.2-24B-Instruct-2506", "mistral3", "mistral")
	require.True(t, ok)
	assert.Equal(t, "mistral", r.Parser)
	assert.Equal(t, []string{"--tokenizer-mode=mistral"}, r.ExtraArgs)

	_, ok = c.ResolveParser("google/gemma-4", "gemma4", "gemma4_text")
	assert.False(t, ok)
}

// A validity guard, not a content freeze: the org list, licences and rules
// change by PR, and this must not force an edit on every one of them.
func TestCommittedAuditConfigLoads(t *testing.T) {
	_, err := LoadConfig("../../../inference/vllm/audit.yaml")
	require.NoError(t, err)
}

// The rules are only trustworthy if they reproduce what already serves.
func TestCommittedRulesResolveTheIncumbentToItsServingParser(t *testing.T) {
	c, err := LoadConfig("../../../inference/vllm/audit.yaml")
	require.NoError(t, err)
	cur, err := LoadCurrent("../../../inference/vllm/serving.yaml")
	require.NoError(t, err)

	r, ok := c.ResolveParser(cur.Repo, "qwen3_moe", "")
	require.True(t, ok)
	assert.Equal(t, cur.ToolCallParser, r.Parser)
}
