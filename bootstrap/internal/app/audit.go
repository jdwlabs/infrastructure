package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
)

// DefaultAuditConfigPath is audit.yaml relative to the repo root.
const DefaultAuditConfigPath = "inference/vllm/audit.yaml"

// AuditEndpoints points the audit at its upstreams. The zero value is the
// real Hugging Face Hub and GitHub raw content; tests substitute fakes.
type AuditEndpoints struct {
	HubBase string
	RawBase string
	HTTP    *http.Client
	Sleep   func(context.Context, time.Duration) error
	// JiraHTTP reaches JIRA_BASE_URL; nil is a default client.
	JiraHTTP *http.Client
}

// AuditOptions carries what `talops vllm audit` accepts.
type AuditOptions struct {
	Spec      string
	Config    string
	JSON      bool
	DryRun    bool
	Out       io.Writer
	Endpoints AuditEndpoints
	Getenv    func(string) string
	Now       func() time.Time
}

// jiraEnv names the credentials in the order a missing one is reported.
var jiraEnv = []string{"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"}

// RunVLLMAudit lists newly released models that could replace the one
// serving.yaml serves and files them to Jira. It reads the Hub and GitHub
// and never touches the GPU host, the vault or git. A dry run performs every
// read, Jira's included when credentials are present, and no write.
func (app *App) RunVLLMAudit(ctx context.Context, opts AuditOptions) error {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	var missing []string
	for _, k := range jiraEnv {
		if opts.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if !opts.DryRun && len(missing) > 0 {
		return app.emitAudit(opts, failedAudit("jira_unconfigured",
			fmt.Sprintf("%s unset: filing needs JIRA_BASE_URL, JIRA_EMAIL and JIRA_API_TOKEN (or pass --dry-run)", strings.Join(missing, ", "))))
	}

	specPath, cfgPath, failure := auditPaths(opts)
	if failure != nil {
		return app.emitAudit(opts, failedAudit(failure.Code, failure.Msg))
	}
	cur, err := modelaudit.LoadCurrent(specPath)
	if err != nil {
		return app.emitAudit(opts, failedAudit("spec_unreadable", fmt.Sprintf("read %s: %v", specPath, err)))
	}
	cfg, err := modelaudit.LoadConfig(cfgPath)
	if err != nil {
		return app.emitAudit(opts, failedAudit("config_unreadable", fmt.Sprintf("read %s: %v", cfgPath, err)))
	}

	deps := modelaudit.Deps{Now: now(), DryRun: opts.DryRun}
	var api *jira.Client
	var history modelaudit.History
	if len(missing) == 0 {
		api = &jira.Client{
			Base:     strings.TrimRight(opts.Getenv("JIRA_BASE_URL"), "/"),
			Email:    opts.Getenv("JIRA_EMAIL"),
			Token:    opts.Getenv("JIRA_API_TOKEN"),
			HTTP:     opts.Endpoints.JiraHTTP,
			ReadOnly: opts.DryRun,
		}
		// Read before any Hub request: filing without dedupe would repeat
		// candidates, so a failed read must stop the run while it is cheap.
		history, err = modelaudit.LoadHistory(ctx, api, cfg.Jira.Project)
		if err != nil {
			return app.emitAudit(opts, failedAudit("jira_read_failed", err.Error()))
		}
		deps.Reported = history.Reported
	} else {
		deps.DedupeSkipped = "no Jira credentials (" + strings.Join(missing, ", ") + " unset)"
	}

	token := opts.Getenv("HF_TOKEN")
	ep := opts.Endpoints
	deps.HasToken = token != ""
	deps.Hub = &hub.Client{
		HubBase: orDefault(ep.HubBase, hub.DefaultHubBase),
		RawBase: orDefault(ep.RawBase, hub.DefaultRawBase),
		Token:   token,
		HTTP:    ep.HTTP,
		Budget:  hub.NewBudget(cfg.MaxRequests, cfg.DiscoveryRequests),
		Sleep:   ep.Sleep,
	}
	report, err := modelaudit.Run(ctx, cfg, cur, deps)
	if err != nil || api == nil {
		return app.emitAudit(opts, report)
	}

	if err := modelaudit.File(ctx, api, cfg, &report, history, deps.Now, opts.DryRun); err != nil {
		code := "jira_read_failed"
		var fe *modelaudit.FileError
		if errors.As(err, &fe) && fe.Write {
			code = "jira_write_failed"
		}
		report.Failure = &modelaudit.Failure{Code: code, Msg: err.Error()}
	}
	return app.emitAudit(opts, report)
}

func auditPaths(opts AuditOptions) (spec, cfg string, failure *modelaudit.Failure) {
	spec, cfg = opts.Spec, opts.Config
	if spec != "" && cfg != "" {
		return spec, cfg, nil
	}
	root, err := findRepoRoot()
	if err != nil {
		return "", "", &modelaudit.Failure{Code: "repo_root_unresolved", Msg: fmt.Sprintf("find the repo root for %s and %s: %v", DefaultSpecPath, DefaultAuditConfigPath, err)}
	}
	if spec == "" {
		spec = filepath.Join(root, filepath.FromSlash(DefaultSpecPath))
	}
	if cfg == "" {
		cfg = filepath.Join(root, filepath.FromSlash(DefaultAuditConfigPath))
	}
	return spec, cfg, nil
}

func failedAudit(code, msg string) modelaudit.Report {
	return modelaudit.Report{
		Jira:    modelaudit.JiraOutcome{Action: "not-filed"},
		Failure: &modelaudit.Failure{Code: code, Msg: msg},
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// emitAudit prints the report and returns its failure, so the exit code is 1
// exactly when the report says nothing may be filed.
func (app *App) emitAudit(opts AuditOptions, r modelaudit.Report) error {
	w := opts.Out
	if w == nil {
		w = io.Discard
	}
	if opts.JSON {
		out, err := modelaudit.RenderJSON(r)
		if err != nil {
			return err
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
	} else if _, err := io.WriteString(w, modelaudit.RenderTOON(r)); err != nil {
		return err
	}
	if r.Failure != nil {
		return r.Failure
	}
	return nil
}
