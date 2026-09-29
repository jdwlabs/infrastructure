package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
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

// RunVLLMAudit lists newly released models that could replace the one
// serving.yaml serves. It reads the Hub and GitHub and never touches the GPU
// host, the vault or git.
func (app *App) RunVLLMAudit(ctx context.Context, opts AuditOptions) error {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	if !opts.DryRun {
		return app.emitAudit(opts, failedAudit("jira_not_built", "Jira filing not built yet: run talops vllm audit --dry-run"))
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

	token := opts.Getenv("HF_TOKEN")
	ep := opts.Endpoints
	report, _ := modelaudit.Run(ctx, cfg, cur, modelaudit.Deps{
		Hub: &hub.Client{
			HubBase: orDefault(ep.HubBase, hub.DefaultHubBase),
			RawBase: orDefault(ep.RawBase, hub.DefaultRawBase),
			Token:   token,
			HTTP:    ep.HTTP,
			Budget:  hub.NewBudget(cfg.MaxRequests, cfg.DiscoveryRequests),
			Sleep:   ep.Sleep,
		},
		HasToken:      token != "",
		Now:           now(),
		DryRun:        opts.DryRun,
		DedupeSkipped: "Jira reads are not built yet",
	})
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
