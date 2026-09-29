package modelaudit

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func adf(t *testing.T, n jira.Node) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(n)
	require.NoError(t, err)
	return b
}

func jiraClient(j *audittest.Jira) *jira.Client {
	return &jira.Client{Base: j.Server.URL, Email: "bot@example.com", Token: "t", HTTP: j.Server.Client()}
}

func TestWeekLabelUsesTheISOWeek(t *testing.T) {
	assert.Equal(t, "model-audit-2026-W01", WeekLabel(time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC)))
	assert.Equal(t, "model-audit-2026-W53", WeekLabel(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)))
	assert.Equal(t, "model-audit-2026-W40", WeekLabel(audittest.Now))
}

func TestHistoryQueries(t *testing.T) {
	assert.Equal(t, "project = AUDIT AND labels = model-audit ORDER BY created DESC", historyJQL("AUDIT"))
	assert.Equal(t, `project = AUDIT AND labels = "model-audit-2026-W40"`, weekJQL("AUDIT", "model-audit-2026-W40"))
}

func TestLoadHistoryReadsDescriptionsAndComments(t *testing.T) {
	j := audittest.NewJira(t)
	j.Issues = []*audittest.Issue{
		{Key: "AUDIT-3", Labels: []string{"model-audit", "model-audit-2026-W39"},
			Description: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Qwen/A"}))),
			Comments:    []audittest.Comment{{Author: "someone", Body: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Qwen/B"})))}}},
		{Key: "AUDIT-2", Labels: []string{"model-audit", "model-audit-2026-W38"},
			Description: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Org/C"})))},
		{Key: "AUDIT-1", Labels: []string{"unrelated"},
			Description: adf(t, jira.Doc(jira.CandidatesBlock([]string{"Org/Never"})))},
	}

	h, err := LoadHistory(context.Background(), jiraClient(j), "AUDIT")

	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"Qwen/A": true, "Qwen/B": true, "Org/C": true}, h.Reported)
	require.NotNil(t, h.Latest)
	assert.Equal(t, "AUDIT-3", h.Latest.Key)
	assert.Len(t, h.Latest.Comments, 1)
	assert.Zero(t, j.Posts())
}

func TestLoadHistoryWithNoTicketsHasNoLatest(t *testing.T) {
	h, err := LoadHistory(context.Background(), jiraClient(audittest.NewJira(t)), "AUDIT")
	require.NoError(t, err)
	assert.Nil(t, h.Latest)
	assert.Empty(t, h.Reported)
}

func TestLoadHistoryFailsOnASearchError(t *testing.T) {
	j := audittest.NewJira(t)
	j.SearchStatus = http.StatusInternalServerError
	_, err := LoadHistory(context.Background(), jiraClient(j), "AUDIT")
	assert.ErrorContains(t, err, "HTTP 500")
}
