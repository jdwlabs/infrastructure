package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	committedSpec  = "../../../inference/vllm/serving.yaml"
	committedAudit = "../../../inference/vllm/audit.yaml"
)

type auditEnv struct {
	hub  *audittest.Hub
	jira *audittest.Jira
	cfg  string
	env  map[string]string
	now  time.Time
	// ctx and hubTransport, when set, replace a background context and the
	// fake Hub's own transport.
	ctx          context.Context
	hubTransport http.RoundTripper
}

// newAuditEnv runs against the committed audit.yaml narrowed to two orgs, so
// each case lists only the models it sets up.
func newAuditEnv(t *testing.T, edits ...func(string) string) *auditEnv {
	t.Helper()
	raw, err := os.ReadFile(committedAudit)
	require.NoError(t, err)
	text := regexp.MustCompile(`(?s)orgs: \[.*?\]`).ReplaceAllString(string(raw), "orgs: [Qwen, QuantTrio]")
	for _, e := range edits {
		text = e(text)
	}
	cfg := filepath.Join(t.TempDir(), "audit.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte(text), 0o644))

	e := &auditEnv{hub: audittest.NewHub(t), jira: audittest.NewJira(t), cfg: cfg, now: audittest.Now}
	e.env = map[string]string{
		"JIRA_BASE_URL": e.jira.Server.URL + "/", "JIRA_EMAIL": "bot@example.com", "JIRA_API_TOKEN": "t",
	}
	return e
}

func set(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}

func (e *auditEnv) run(t *testing.T, dryRun bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	hubHTTP := e.hub.Server.Client()
	if e.hubTransport != nil {
		hubHTTP = &http.Client{Transport: e.hubTransport}
	}
	err := New("test").RunVLLMAudit(ctx, AuditOptions{
		Spec: committedSpec, Config: e.cfg, DryRun: dryRun, Out: &out,
		Endpoints: AuditEndpoints{
			HubBase: e.hub.Server.URL, RawBase: e.hub.Server.URL, HTTP: hubHTTP,
			Sleep:    func(context.Context, time.Duration) error { return nil },
			JiraHTTP: e.jira.Server.Client(),
		},
		Getenv: func(k string) string { return e.env[k] },
		Now:    func() time.Time { return e.now },
	})
	return out.String(), err
}

func newModel(id, sha string, dl int64) audittest.Model {
	return audittest.Model{ID: id, SHA: sha, CreatedAt: audittest.Now.Add(-48 * time.Hour), Downloads: dl,
		PipelineTag: "text-generation", License: "apache-2.0"}
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func failureCode(t *testing.T, err error) string {
	t.Helper()
	var f *modelaudit.Failure
	require.ErrorAs(t, err, &f)
	return f.Code
}

func TestAuditNormalWeekCreatesExactlyOneTicket(t *testing.T) {
	e := newAuditEnv(t)
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}

	out, err := e.run(t, false)

	require.NoError(t, err, out)
	assert.Equal(t, 1, e.jira.Posts())
	assert.Len(t, e.jira.Created(), 1)
	assert.Contains(t, out, "jira:\n  action: created\n  key: AUDIT-101\n")
}

func TestAuditNothingNewCommentsOnceEvenWhenRunTwice(t *testing.T) {
	e := newAuditEnv(t)
	e.jira.Issues = []*audittest.Issue{{Key: "AUDIT-6", Labels: []string{"model-audit", "model-audit-2026-W39"}}}

	_, err := e.run(t, false)
	require.NoError(t, err)
	out, err := e.run(t, false)
	require.NoError(t, err)

	assert.Equal(t, 1, e.jira.Posts())
	require.Len(t, e.jira.Issues[0].Comments, 1)
	assert.Contains(t, out, "  action: none\n  key: AUDIT-6\n")
}

func TestAuditSameWeekRerunCommentsThenNextWeekDedupes(t *testing.T) {
	e := newAuditEnv(t, set("windowDays: 7", "windowDays: 14"))
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}
	_, err := e.run(t, false)
	require.NoError(t, err)

	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/B-Instruct", shaB, 20), newModel("Qwen/A-Instruct", shaA, 10)}}
	out, err := e.run(t, false)
	require.NoError(t, err, out)
	assert.Contains(t, out, "  action: commented\n  key: AUDIT-101\n")
	week := e.jira.Issues[0]
	require.Len(t, week.Comments, 1)
	assert.Equal(t, []string{"Qwen/B-Instruct"}, jira.CandidateRepos(week.Comments[0].Body))

	e.now = audittest.Now.Add(7 * 24 * time.Hour)
	out, err = e.run(t, false)
	require.NoError(t, err, out)
	assert.Contains(t, out, "candidates[0]{")
	assert.Contains(t, out, "Qwen/A-Instruct,already reported")
	assert.Contains(t, out, "Qwen/B-Instruct,already reported")
	assert.Contains(t, out, "  action: commented\n  key: AUDIT-101\n")
	assert.Len(t, e.jira.Created(), 1, "no second ticket")
}

func TestAuditDedupeSearchFailureFilesNothing(t *testing.T) {
	e := newAuditEnv(t)
	e.jira.SearchStatus = http.StatusInternalServerError
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}

	out, err := e.run(t, false)

	assert.Equal(t, "jira_read_failed", failureCode(t, err))
	assert.Contains(t, out, "error: {code: jira_read_failed")
	assert.Zero(t, e.jira.Posts())
	assert.Empty(t, e.hub.Requests(), "the Hub budget is not spent on a run that cannot file")
}

func TestAuditDiscoveryBudgetSpentBeforeAnyOrgFilesNothing(t *testing.T) {
	e := newAuditEnv(t, set("discoveryRequests: 120", "discoveryRequests: 1"))

	_, err := e.run(t, false)

	assert.Equal(t, "discovery_budget_spent", failureCode(t, err))
	assert.Zero(t, e.jira.Posts())
}

// cancelBefore cancels the run's context just before the first Hub request
// that matches, the way a SIGTERM lands mid-run.
type cancelBefore struct {
	next   http.RoundTripper
	match  func(*http.Request) bool
	cancel context.CancelFunc
}

func (c cancelBefore) RoundTrip(req *http.Request) (*http.Response, error) {
	if c.match(req) {
		c.cancel()
	}
	return c.next.RoundTrip(req)
}

func cancelOn(e *auditEnv, match func(*http.Request) bool) {
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.hubTransport = cancelBefore{next: e.hub.Server.Client().Transport, match: match, cancel: cancel}
}

func TestAuditCancelledDuringDiscoveryFailsAndFilesNothing(t *testing.T) {
	e := newAuditEnv(t)
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}
	e.hub.Orgs["QuantTrio"] = [][]audittest.Model{{newModel("QuantTrio/B-Instruct", shaB, 10)}}
	cancelOn(e, func(r *http.Request) bool { return r.URL.Query().Get("author") == "QuantTrio" })

	out, err := e.run(t, false)

	assert.Equal(t, "cancelled", failureCode(t, err), out)
	assert.Contains(t, out, "error: {code: cancelled")
	assert.Zero(t, e.jira.Posts())
	for _, r := range e.hub.Requests() {
		assert.NotContains(t, r, "/revision/", "nothing is enriched once the run is cancelled")
	}
}

func TestAuditCancelledDuringEnrichmentFailsAndFilesNothing(t *testing.T) {
	e := newAuditEnv(t)
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 20), newModel("Qwen/B-Instruct", shaB, 10)}}
	cancelOn(e, func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/models/Qwen/A-Instruct/") })

	out, err := e.run(t, false)

	assert.Equal(t, "cancelled", failureCode(t, err), out)
	assert.Contains(t, out, "error: {code: cancelled")
	assert.Zero(t, e.jira.Posts())
	assert.NotContains(t, out, "enrich failed: ", "a signal is not a per-candidate rejection")
}

func TestAuditEnrichmentBudgetSpentStillFilesAndNamesTheRest(t *testing.T) {
	// registry, two org pages, trending, then three enrich requests for A
	e := newAuditEnv(t, set("maxRequests: 400", "maxRequests: 7"), set("discoveryRequests: 120", "discoveryRequests: 6"))
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 20), newModel("Qwen/B-Instruct", shaB, 10)}}

	out, err := e.run(t, false)

	require.NoError(t, err, out)
	assert.Contains(t, out, "Qwen/B-Instruct,not enriched: request budget spent")
	assert.Contains(t, out, "enrichment,1 candidates not enriched: request budget spent")
	assert.Contains(t, out, "  action: created\n")
	assert.Contains(t, string(e.jira.Created()[0]), "1 candidates not enriched")
}

func TestAuditFailedAndTruncatedOrgsAreFiledAsSkipped(t *testing.T) {
	// registry, four attempts at Qwen, QuantTrio's first page
	e := newAuditEnv(t, set("discoveryRequests: 120", "discoveryRequests: 6"))
	e.hub.OrgStatus["Qwen"] = http.StatusInternalServerError
	e.hub.Orgs["QuantTrio"] = [][]audittest.Model{{newModel("QuantTrio/A-Instruct", shaA, 1)}, {newModel("QuantTrio/B-Instruct", shaB, 1)}}

	out, err := e.run(t, false)

	require.NoError(t, err, out)
	desc := string(e.jira.Created()[0])
	assert.Contains(t, desc, "Sources skipped")
	assert.Contains(t, desc, "QuantTrio: truncated at 1")
	assert.Contains(t, desc, "trending: not read: request budget spent")
	assert.Regexp(t, `Qwen: GET [^"]*HTTP 500`, desc)
}

func TestAuditDryRunWithCredentialsReadsJiraButNeverWrites(t *testing.T) {
	e := newAuditEnv(t)
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}

	out, err := e.run(t, true)

	require.NoError(t, err, out)
	assert.NotEmpty(t, e.jira.Calls())
	assert.Zero(t, e.jira.Posts())
	assert.Contains(t, out, "  action: would-create\n")
	assert.Contains(t, out, "  dryRun: true\n")
}

func TestAuditDryRunWithoutCredentialsMakesNoJiraRequest(t *testing.T) {
	e := newAuditEnv(t)
	e.env = map[string]string{"JIRA_EMAIL": "bot@example.com"}

	out, err := e.run(t, true)

	require.NoError(t, err, out)
	assert.Empty(t, e.jira.Calls())
	assert.Contains(t, out, "jira,\"dedupe skipped: no Jira credentials (JIRA_BASE_URL, JIRA_API_TOKEN unset)\"")
	assert.Contains(t, out, "  action: not-filed\n")
}

func TestAuditWithoutCredentialsOrDryRunRefusesBeforeAnyRequest(t *testing.T) {
	e := newAuditEnv(t)
	e.env = map[string]string{"JIRA_BASE_URL": e.jira.Server.URL, "JIRA_EMAIL": "bot@example.com"}

	out, err := e.run(t, false)

	assert.Equal(t, "jira_unconfigured", failureCode(t, err))
	assert.Contains(t, out, "JIRA_API_TOKEN unset")
	assert.Empty(t, e.jira.Calls())
	assert.Empty(t, e.hub.Requests())
}

func TestAuditJiraWriteFailureExitsOne(t *testing.T) {
	e := newAuditEnv(t)
	e.jira.WriteStatus = http.StatusBadRequest
	e.hub.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/A-Instruct", shaA, 10)}}

	out, err := e.run(t, false)

	assert.Equal(t, "jira_write_failed", failureCode(t, err))
	assert.Contains(t, out, "error: {code: jira_write_failed")
}

func TestAuditJSONIsOneObjectWithTheJiraOutcome(t *testing.T) {
	e := newAuditEnv(t)
	var out bytes.Buffer
	err := New("test").RunVLLMAudit(context.Background(), AuditOptions{
		Spec: committedSpec, Config: e.cfg, DryRun: true, JSON: true, Out: &out,
		Endpoints: AuditEndpoints{HubBase: e.hub.Server.URL, RawBase: e.hub.Server.URL, HTTP: e.hub.Server.Client(), JiraHTTP: e.jira.Server.Client()},
		Getenv:    func(k string) string { return e.env[k] },
		Now:       func() time.Time { return audittest.Now },
	})
	require.NoError(t, err)
	var obj struct {
		Event string                 `json:"event"`
		Jira  modelaudit.JiraOutcome `json:"jira"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &obj))
	assert.Equal(t, "audit", obj.Event)
	assert.Equal(t, "would-create", obj.Jira.Action)
}

func TestAuditRefusesUnreadableInputsBeforeAnyRequest(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("windowDays: 7\nunknownKey: 1\n"), 0o644))
	for name, c := range map[string]struct {
		spec, cfg, code string
	}{
		"spec missing":   {filepath.Join(t.TempDir(), "none.yaml"), "", "spec_unreadable"},
		"config invalid": {committedSpec, bad, "config_unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newAuditEnv(t)
			cfg := c.cfg
			if cfg == "" {
				cfg = e.cfg
			}
			var out bytes.Buffer
			err := New("test").RunVLLMAudit(context.Background(), AuditOptions{
				Spec: c.spec, Config: cfg, DryRun: true, Out: &out,
				Endpoints: AuditEndpoints{HubBase: e.hub.Server.URL, RawBase: e.hub.Server.URL, HTTP: e.hub.Server.Client(), JiraHTTP: e.jira.Server.Client()},
				Getenv:    func(k string) string { return e.env[k] },
			})
			assert.Equal(t, c.code, failureCode(t, err))
			assert.Empty(t, e.hub.Requests())
			assert.Empty(t, e.jira.Calls())
		})
	}
}
