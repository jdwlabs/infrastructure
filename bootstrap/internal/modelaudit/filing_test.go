package modelaudit

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withCandidates(repos ...string) Report {
	r := adfReport()
	r.Candidates = nil
	for _, repo := range repos {
		c := goldenReport().Candidates[0]
		c.Repo = repo
		r.Candidates = append(r.Candidates, c)
	}
	return r
}

func history(t *testing.T, j *audittest.Jira) History {
	t.Helper()
	h, err := LoadHistory(context.Background(), jiraClient(j), "AUDIT")
	require.NoError(t, err)
	return h
}

func TestFileCreatesTheWeeksTicket(t *testing.T) {
	j := audittest.NewJira(t)
	r := withCandidates("Qwen/A")

	err := File(context.Background(), jiraClient(j), testConfig(t), &r, history(t, j), audittest.Now, false)

	require.NoError(t, err)
	assert.Equal(t, JiraOutcome{Action: "created", Key: "AUDIT-101"}, r.Jira)
	require.Len(t, j.Created(), 1)
	var fields struct {
		Summary   string            `json:"summary"`
		Labels    []string          `json:"labels"`
		Parent    map[string]string `json:"parent"`
		Project   map[string]string `json:"project"`
		IssueType map[string]string `json:"issuetype"`
	}
	require.NoError(t, json.Unmarshal(j.Created()[0], &fields))
	assert.Equal(t, "Review 1 local model candidates for 2026-W40", fields.Summary)
	assert.Equal(t, []string{"model-audit", "model-audit-2026-W40", "upgrade", "monitoring"}, fields.Labels)
	assert.Equal(t, "AUDIT-1", fields.Parent["key"])
	assert.Equal(t, "AUDIT", fields.Project["key"])
	assert.Equal(t, "Task", fields.IssueType["name"])
}

func TestFileCommentsOnlyTheNewCandidatesOnARerun(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{{Key: "AUDIT-7", Labels: []string{"model-audit", "model-audit-2026-W40"},
		Description: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Qwen/A"})))}}
	r := withCandidates("Qwen/A", "Qwen/B")

	err := File(context.Background(), jiraClient(j), testConfig(t), &r, History{}, audittest.Now, false)

	require.NoError(t, err)
	assert.Equal(t, JiraOutcome{Action: "commented", Key: "AUDIT-7"}, r.Jira)
	require.Len(t, j.Issues[0].Comments, 1)
	assert.Equal(t, []string{"Qwen/B"}, jira.CandidateRepos(j.Issues[0].Comments[0].Body))
}

func TestFileDoesNothingWhenTheWeeksTicketListsEveryCandidate(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{{Key: "AUDIT-7", Labels: []string{"model-audit", "model-audit-2026-W40"},
		Description: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Qwen/A"})))}}
	r := withCandidates("Qwen/A")

	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r, History{}, audittest.Now, false))

	assert.Equal(t, JiraOutcome{Action: "none", Key: "AUDIT-7"}, r.Jira)
	assert.Zero(t, j.Posts())
}

func TestFileNoChangeCommentsOnceOnTheLatestTicket(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{{Key: "AUDIT-6", Labels: []string{"model-audit", "model-audit-2026-W39"}}}
	r := withCandidates()

	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r, history(t, j), audittest.Now, false))
	assert.Equal(t, JiraOutcome{Action: "commented", Key: "AUDIT-6"}, r.Jira)

	r2 := withCandidates()
	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r2, history(t, j), audittest.Now, false))
	assert.Equal(t, JiraOutcome{Action: "none", Key: "AUDIT-6"}, r2.Jira)

	require.Len(t, j.Issues[0].Comments, 1, "nothing new twice in a week is one comment")
	assert.Contains(t, jira.PlainTextOf(j.Issues[0].Comments[0].Body), "No new candidates in 2026-W40 (model-audit-2026-W40)")
}

// A human quoting the label is not the audit having said it.
func TestFileNoChangeIgnoresAHumansCommentNamingTheWeek(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{{Key: "AUDIT-6", Labels: []string{"model-audit", "model-audit-2026-W39"},
		Comments: []audittest.Comment{{Author: "a-human", Body: adf(t, jira.Doc(jira.Paragraph(jira.Text("waiting on model-audit-2026-W40"))))}}}}
	r := withCandidates()

	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r, history(t, j), audittest.Now, false))

	assert.Equal(t, "commented", r.Jira.Action)
}

func TestFileNoChangeSkipsThisWeeksOwnTicket(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{{Key: "AUDIT-8", Labels: []string{"model-audit", "model-audit-2026-W40"}}}
	r := withCandidates()

	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r, history(t, j), audittest.Now, false))

	assert.Equal(t, JiraOutcome{Action: "none", Key: "AUDIT-8"}, r.Jira)
	assert.Zero(t, j.Posts())
}

func TestFileWithNoTicketAtAllCreatesAZeroCandidateOne(t *testing.T) {
	j := audittest.NewJira(t)
	r := withCandidates()

	require.NoError(t, File(context.Background(), jiraClient(j), testConfig(t), &r, history(t, j), audittest.Now, false))

	assert.Equal(t, "created", r.Jira.Action)
	var fields struct {
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(j.Created()[0], &fields))
	assert.Equal(t, "Review 0 local model candidates for 2026-W40", fields.Summary)
}

func TestFileDryRunReadsButNeverWrites(t *testing.T) {
	for name, setup := range map[string]func(*audittest.Jira) Report{
		"would create": func(*audittest.Jira) Report { return withCandidates("Qwen/A") },
		"would comment new candidates": func(j *audittest.Jira) Report {
			j.Issues = []*audittest.Issue{{Key: "AUDIT-7", Labels: []string{"model-audit", "model-audit-2026-W40"}}}
			return withCandidates("Qwen/A")
		},
		"would comment no change": func(j *audittest.Jira) Report {
			j.Issues = []*audittest.Issue{{Key: "AUDIT-6", Labels: []string{"model-audit", "model-audit-2026-W39"}}}
			return withCandidates()
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := audittest.NewJira(t)
			r := setup(j)
			c := jiraClient(j)
			c.ReadOnly = true

			require.NoError(t, File(context.Background(), c, testConfig(t), &r, history(t, j), audittest.Now, true))

			assert.Contains(t, []string{"would-create", "would-comment"}, r.Jira.Action)
			assert.Zero(t, j.Posts())
		})
	}
}

func TestFileErrorsSayWhetherAWriteFailed(t *testing.T) {
	j := audittest.NewJira(t)
	j.WriteStatus = http.StatusBadRequest
	r := withCandidates("Qwen/A")
	err := File(context.Background(), jiraClient(j), testConfig(t), &r, History{}, audittest.Now, false)
	var fe *FileError
	require.ErrorAs(t, err, &fe)
	assert.True(t, fe.Write)

	j2 := audittest.NewJira(t)
	j2.SearchStatus = http.StatusInternalServerError
	r = withCandidates("Qwen/A")
	err = File(context.Background(), jiraClient(j2), testConfig(t), &r, History{}, audittest.Now, false)
	require.ErrorAs(t, err, &fe)
	assert.False(t, fe.Write)
	assert.Zero(t, j2.Posts())
}
