package modelaudit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
)

// FileError distinguishes a failed read, which filed nothing, from a failed
// write, which may have: the exit code is 1 either way, the code differs.
type FileError struct {
	Write bool
	Err   error
}

func (e *FileError) Error() string { return e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

// File records the report in Jira, idempotently per ISO week:
//   - candidates and no ticket with this week's label: create one;
//   - candidates and this week's ticket exists: comment only the new ones;
//   - no candidates: one no-change comment on the newest ticket, unless it
//     is this week's own report or the audit's account already said so.
//
// With dryRun it performs the same reads and reports what it would write.
func File(ctx context.Context, api JiraAPI, cfg Config, r *Report, h History, now time.Time, dryRun bool) error {
	label := WeekLabel(now)
	week := Week(now)

	if len(r.Candidates) > 0 {
		found, err := api.Search(ctx, weekJQL(cfg.Jira.Project, label), 1)
		if err != nil {
			return &FileError{Err: fmt.Errorf("search this week's ticket: %w", err)}
		}
		if len(found) == 0 {
			return create(ctx, api, cfg, r, label, week, dryRun)
		}
		t, err := loadTicket(ctx, api, found[0])
		if err != nil {
			return &FileError{Err: err}
		}
		var fresh []CandidateRow
		for _, c := range r.Candidates {
			if !t.Repos[c.Repo] {
				fresh = append(fresh, c)
			}
		}
		if len(fresh) == 0 {
			r.Jira = JiraOutcome{Action: "none", Key: t.Key}
			return nil
		}
		if dryRun {
			r.Jira = JiraOutcome{Action: "would-comment", Key: t.Key}
			return nil
		}
		if err := api.AddComment(ctx, t.Key, CandidatesCommentADF(fresh, week, r.Summary.BudgetGiB)); err != nil {
			return &FileError{Write: true, Err: fmt.Errorf("comment on %s: %w", t.Key, err)}
		}
		r.Jira = JiraOutcome{Action: "commented", Key: t.Key}
		return nil
	}

	if h.Latest == nil {
		return create(ctx, api, cfg, r, label, week, dryRun)
	}
	latest := h.Latest
	if slices.Contains(latest.Labels, label) {
		r.Jira = JiraOutcome{Action: "none", Key: latest.Key}
		return nil
	}
	me, err := api.Myself(ctx)
	if err != nil {
		return &FileError{Err: fmt.Errorf("read the audit's own account: %w", err)}
	}
	for _, c := range latest.Comments {
		if c.Author.AccountID == me && strings.Contains(jira.PlainTextOf(c.Body), label) {
			r.Jira = JiraOutcome{Action: "none", Key: latest.Key}
			return nil
		}
	}
	if dryRun {
		r.Jira = JiraOutcome{Action: "would-comment", Key: latest.Key}
		return nil
	}
	if err := api.AddComment(ctx, latest.Key, NoChangeCommentADF(*r, label)); err != nil {
		return &FileError{Write: true, Err: fmt.Errorf("comment on %s: %w", latest.Key, err)}
	}
	r.Jira = JiraOutcome{Action: "commented", Key: latest.Key}
	return nil
}

func create(ctx context.Context, api JiraAPI, cfg Config, r *Report, label, week string, dryRun bool) error {
	if dryRun {
		r.Jira = JiraOutcome{Action: "would-create"}
		return nil
	}
	key, err := api.CreateIssue(ctx, jira.CreateInput{
		Project:     cfg.Jira.Project,
		IssueType:   cfg.Jira.IssueType,
		Parent:      cfg.Jira.Parent,
		Summary:     summary(len(r.Candidates), week),
		Labels:      []string{"model-audit", label, "upgrade", "monitoring"},
		Description: DescriptionADF(*r),
	})
	if err != nil {
		return &FileError{Write: true, Err: fmt.Errorf("create the weekly ticket: %w", err)}
	}
	r.Jira = JiraOutcome{Action: "created", Key: key}
	return nil
}

func summary(n int, week string) string {
	noun := "candidates"
	if n == 1 {
		noun = "candidate"
	}
	return fmt.Sprintf("Review %d local model %s for %s", n, noun, week)
}
