package modelaudit

import (
	"fmt"
	"time"
)

// PublicHubURL is where report links point, whatever base the client used.
const PublicHubURL = "https://huggingface.co"

// TrialStepsNote heads every candidate's trial steps.
const TrialStepsNote = "trial steps are a starting point pending the model-trial procedure"

type Pool string

const (
	PoolAllowListed Pool = "allow-listed"
	PoolUnvetted    Pool = "unvetted"
)

// Report is the audit's one output: printed as TOON or JSON, and filed to
// Jira. Its field names and order are a public contract.
type Report struct {
	Summary    Summary        `json:"summary"`
	Candidates []CandidateRow `json:"candidates"`
	Reasons    []ReasonCount  `json:"rejectionCounts"`
	Rejected   []Rejection    `json:"rejected"`
	Skipped    []Skip         `json:"skipped"`
	Jira       JiraOutcome    `json:"jira"`
	Failure    *Failure       `json:"error,omitempty"`
}

type Summary struct {
	Week       string  `json:"week"`
	Model      string  `json:"model"`
	ServedName string  `json:"servedName"`
	VLLMTag    string  `json:"vllm"`
	BudgetGiB  float64 `json:"budgetGiB"`
	Checked    int     `json:"checked"`
	Candidates int     `json:"candidates"`
	Rejected   int     `json:"rejected"`
	Requests   int     `json:"requests"`
	DryRun     bool    `json:"dryRun"`
}

type CandidateRow struct {
	Repo       string    `json:"repo"`
	SHA        string    `json:"sha"`
	Pool       Pool      `json:"pool"`
	CreatedAt  time.Time `json:"createdAt"`
	Downloads  int64     `json:"downloads"`
	Quant      string    `json:"quant"`
	WeightsGiB float64   `json:"weightsGiB"`
	TotalGiB   float64   `json:"totalGiB"`
	Parser     string    `json:"parser"`
	License    string    `json:"license"`
	Gated      string    `json:"gated"`
	URL        string    `json:"url"`
	TrialSteps []string  `json:"trialSteps"`
}

type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type Rejection struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

type Skip struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// JiraOutcome says what the run did in Jira: created, commented, none,
// would-create, would-comment, or not-filed (a dry run with no Jira at all).
type JiraOutcome struct {
	Action string `json:"action"`
	Key    string `json:"key"`
}

// Failure is a run that must not file anything; the report still prints.
type Failure struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (f *Failure) Error() string { return f.Msg }

// Week is the ISO week in UTC: the ISO year, not the calendar year, so
// 2025-12-29 is 2026-W01 and 2027-01-01 is 2026-W53.
func Week(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}
