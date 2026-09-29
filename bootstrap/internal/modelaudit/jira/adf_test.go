package jira

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCandidateReposRoundTripsTheBlock(t *testing.T) {
	doc := Doc(
		Paragraph(Text("intro")),
		BulletList([]Node{Text("nested:")}),
		CandidatesBlock([]string{"Qwen/A", "Org/B"}),
		CodeBlock("go", "not/ours"),
	)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	assert.Equal(t, []string{"Qwen/A", "Org/B"}, CandidateRepos(raw))
}

// The Jira editor may normalise an unknown language away; the header line
// still identifies the block.
func TestCandidateReposMatchesOnTheHeaderWhenTheLanguageIsGone(t *testing.T) {
	raw := json.RawMessage(`{"type":"doc","version":1,"content":[
	  {"type":"panel","content":[
	    {"type":"codeBlock","attrs":{"language":"plaintext"},"content":[{"type":"text","text":"# audit-candidates\nQwen/A\n\n  Org/B  "}]}
	  ]}]}`)

	assert.Equal(t, []string{"Qwen/A", "Org/B"}, CandidateRepos(raw))
}

func TestCandidateReposIgnoresOtherBlocksAndNonADF(t *testing.T) {
	assert.Empty(t, CandidateRepos(json.RawMessage(`{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"Qwen/A"}]}]}`)))
	assert.Empty(t, CandidateRepos(json.RawMessage(`"a plain string description"`)))
	assert.Empty(t, CandidateRepos(nil))
}

func TestPlainTextOfFlattensEveryTextNode(t *testing.T) {
	raw, err := json.Marshal(Doc(Paragraph(Text("No new candidates in "), Text("2026-W40")), BulletList([]Node{Link("x", "https://e")})))
	require.NoError(t, err)
	assert.Equal(t, "No new candidates in 2026-W40x", PlainTextOf(raw))
}
