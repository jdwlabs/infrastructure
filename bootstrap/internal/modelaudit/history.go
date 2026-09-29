package modelaudit

import (
	"context"
	"fmt"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
)

// historyDepth is half a year of weekly tickets.
const historyDepth = 26

// JiraAPI is the part of jira.Client the audit calls.
type JiraAPI interface {
	Search(ctx context.Context, jql string, max int) ([]jira.Issue, error)
	Comments(ctx context.Context, key string) ([]jira.Comment, error)
	Myself(ctx context.Context) (string, error)
	CreateIssue(ctx context.Context, in jira.CreateInput) (string, error)
	AddComment(ctx context.Context, key string, body jira.Node) error
}

// Ticket is one earlier model-audit issue and everything it already lists.
type Ticket struct {
	Key      string
	Labels   []string
	Repos    map[string]bool
	Comments []jira.Comment
}

// History is what the recent model-audit tickets already say. Latest is the
// newest ticket, nil when none exists.
type History struct {
	Reported map[string]bool
	Latest   *Ticket
}

// WeekLabel labels a week's report; one label per ISO week makes a re-run in
// the same week find the ticket it already filed.
func WeekLabel(t time.Time) string { return "model-audit-" + Week(t) }

func historyJQL(project string) string {
	return fmt.Sprintf("project = %s AND labels = model-audit ORDER BY created DESC", project)
}

func weekJQL(project, label string) string {
	return fmt.Sprintf("project = %s AND labels = %q", project, label)
}

// LoadHistory reads the candidate lists from the descriptions and comments
// of the last historyDepth model-audit tickets. Comments count because a
// same-week re-run appends its new candidates as one.
func LoadHistory(ctx context.Context, api JiraAPI, project string) (History, error) {
	issues, err := api.Search(ctx, historyJQL(project), historyDepth)
	if err != nil {
		return History{}, fmt.Errorf("search model-audit history: %w", err)
	}
	h := History{Reported: map[string]bool{}}
	for i, is := range issues {
		t, err := loadTicket(ctx, api, is)
		if err != nil {
			return History{}, err
		}
		for r := range t.Repos {
			h.Reported[r] = true
		}
		if i == 0 {
			h.Latest = t
		}
	}
	return h, nil
}

func loadTicket(ctx context.Context, api JiraAPI, is jira.Issue) (*Ticket, error) {
	comments, err := api.Comments(ctx, is.Key)
	if err != nil {
		return nil, fmt.Errorf("read comments on %s: %w", is.Key, err)
	}
	t := &Ticket{Key: is.Key, Labels: is.Fields.Labels, Repos: map[string]bool{}, Comments: comments}
	for _, r := range jira.CandidateRepos(is.Fields.Description) {
		t.Repos[r] = true
	}
	for _, c := range comments {
		for _, r := range jira.CandidateRepos(c.Body) {
			t.Repos[r] = true
		}
	}
	return t, nil
}
