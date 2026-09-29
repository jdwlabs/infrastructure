package modelaudit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RenderTOON prints the report. Every section prints even when empty, so
// "no candidates" is stated rather than inferred from a missing block.
func RenderTOON(r Report) string {
	var b strings.Builder
	s := r.Summary
	b.WriteString("summary:\n")
	fmt.Fprintf(&b, "  week: %s\n", s.Week)
	fmt.Fprintf(&b, "  model: %s\n", toonCell(s.Model))
	fmt.Fprintf(&b, "  servedName: %s\n", toonCell(s.ServedName))
	fmt.Fprintf(&b, "  vllm: %s\n", toonCell(s.VLLMTag))
	fmt.Fprintf(&b, "  budgetGiB: %.2f\n", s.BudgetGiB)
	fmt.Fprintf(&b, "  checked: %d\n", s.Checked)
	fmt.Fprintf(&b, "  candidates: %d\n", s.Candidates)
	fmt.Fprintf(&b, "  rejected: %d\n", s.Rejected)
	fmt.Fprintf(&b, "  requests: %d\n", s.Requests)
	fmt.Fprintf(&b, "  dryRun: %t\n", s.DryRun)

	fmt.Fprintf(&b, "candidates[%d]{repo,pool,createdAt,downloads,quant,weightsGiB,totalGiB,parser,license,gated,url}:\n", len(r.Candidates))
	var steps int
	for _, c := range r.Candidates {
		fmt.Fprintf(&b, "  %s,%s,%s,%d,%s,%.2f,%.2f,%s,%s,%s,%s\n",
			toonCell(c.Repo), c.Pool, c.CreatedAt.UTC().Format(time.RFC3339), c.Downloads, toonCell(c.Quant),
			c.WeightsGiB, c.TotalGiB, toonCell(c.Parser), toonCell(c.License), toonCell(c.Gated), toonCell(c.URL))
		steps += len(c.TrialSteps)
	}
	if steps > 0 {
		fmt.Fprintf(&b, "note: %s\n", TrialStepsNote)
		fmt.Fprintf(&b, "trialSteps[%d]{repo,step}:\n", steps)
		for _, c := range r.Candidates {
			for _, st := range c.TrialSteps {
				fmt.Fprintf(&b, "  %s,%s\n", toonCell(c.Repo), toonCell(st))
			}
		}
	}

	fmt.Fprintf(&b, "rejectionCounts[%d]{reason,count}:\n", len(r.Reasons))
	for _, rc := range r.Reasons {
		fmt.Fprintf(&b, "  %s,%d\n", toonCell(rc.Reason), rc.Count)
	}
	fmt.Fprintf(&b, "rejected[%d]{repo,reason}:\n", len(r.Rejected))
	for _, x := range r.Rejected {
		fmt.Fprintf(&b, "  %s,%s\n", toonCell(x.Repo), toonCell(x.Reason))
	}
	fmt.Fprintf(&b, "skipped[%d]{source,error}:\n", len(r.Skipped))
	for _, x := range r.Skipped {
		fmt.Fprintf(&b, "  %s,%s\n", toonCell(x.Source), toonCell(x.Error))
	}
	b.WriteString("jira:\n")
	fmt.Fprintf(&b, "  action: %s\n", toonCell(r.Jira.Action))
	fmt.Fprintf(&b, "  key: %s\n", toonCell(r.Jira.Key))
	if r.Failure != nil {
		fmt.Fprintf(&b, "error: {code: %s, msg: %q}\n", r.Failure.Code, r.Failure.Msg)
	}
	return b.String()
}

// RenderJSON prints the report as one object, tagged like the rest of the
// vllm group's JSON so a consumer can tell which command produced it.
func RenderJSON(r Report) ([]byte, error) {
	if r.Candidates == nil {
		r.Candidates = []CandidateRow{}
	}
	for i := range r.Candidates {
		if r.Candidates[i].TrialSteps == nil {
			r.Candidates[i].TrialSteps = []string{}
		}
	}
	if r.Reasons == nil {
		r.Reasons = []ReasonCount{}
	}
	if r.Rejected == nil {
		r.Rejected = []Rejection{}
	}
	if r.Skipped == nil {
		r.Skipped = []Skip{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Trial steps carry <placeholders> a human reads; < helps nobody.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Event string `json:"event"`
		Report
	}{Event: "audit", Report: r}); err != nil {
		return nil, fmt.Errorf("encode audit report: %w", err)
	}
	return buf.Bytes(), nil
}

// toonCell quotes a value that would otherwise break the row's field boundaries.
func toonCell(v string) string {
	if v == "" {
		return "-"
	}
	if strings.ContainsAny(v, ",\"\n") || strings.TrimSpace(v) != v {
		return fmt.Sprintf("%q", v)
	}
	return v
}
