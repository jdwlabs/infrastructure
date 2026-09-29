package modelaudit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adfLines renders a doc with one top-level node per line: compact enough to
// review, and still the exact JSON Jira receives once the newlines go.
func adfLines(t *testing.T, doc jira.Node) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"doc","version":1,"content":[` + "\n")
	for i, n := range doc.Content {
		line, err := json.Marshal(n)
		require.NoError(t, err)
		b.Write(line)
		if i < len(doc.Content)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]}\n")
	return b.String()
}

func adfGolden(t *testing.T, name string, n jira.Node) {
	t.Helper()
	want, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	got := adfLines(t, n)
	assert.Equal(t, string(want), got)
	whole, err := json.Marshal(n)
	require.NoError(t, err)
	assert.JSONEq(t, string(whole), got, "the line format must be the same document")
}

func adfReport() Report {
	r := goldenReport()
	r.Failure = nil
	return r
}

func TestDescriptionADFMatchesGolden(t *testing.T) {
	adfGolden(t, "description.adf.golden.json", DescriptionADF(adfReport()))
}

func TestCandidatesCommentADFMatchesGolden(t *testing.T) {
	r := adfReport()
	adfGolden(t, "comment-candidates.adf.golden.json", CandidatesCommentADF(r.Candidates, "2026-W40", r.Summary.BudgetGiB))
}

func TestNoChangeCommentADFMatchesGolden(t *testing.T) {
	r := adfReport()
	r.Candidates = nil
	adfGolden(t, "comment-no-change.adf.golden.json", NoChangeCommentADF(r, "model-audit-2026-W40"))
}

// Dedupe reads back exactly what filing writes.
func TestDescriptionCandidateBlockRoundTrips(t *testing.T) {
	raw, err := json.Marshal(DescriptionADF(adfReport()))
	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/Qwen3-Coder-Next-Instruct"}, jira.CandidateRepos(raw))
}

func TestDescriptionOmitsSkippedSectionWhenEmpty(t *testing.T) {
	r := adfReport()
	r.Skipped = nil
	raw, err := json.Marshal(DescriptionADF(r))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "Sources skipped")
}
