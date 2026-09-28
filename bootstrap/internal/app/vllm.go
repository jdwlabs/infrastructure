package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/sshutil"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/vllm"
	"go.uber.org/zap"
)

// CodeConfirmRequired is the failure code for an apply attempted without
// --confirm, exported so the cmd layer can recognise it and map the exit
// code to 2 (a usage problem) instead of the default 1 (a failure).
const CodeConfirmRequired = "confirm_required"

// DefaultSpecPath is where inference/vllm/serving.yaml lives relative to the
// repo root. Named in help text when it cannot be found.
const DefaultSpecPath = "inference/vllm/serving.yaml"

// VLLMOptions carries the flags the vllm command group accepts.
type VLLMOptions struct {
	// Host overrides the push target, the same way HAProxyOptions.Host does:
	// verifying a replacement GPU host on a temporary address before tfvars
	// is repointed at it.
	Host    string
	Spec    string
	JSON    bool
	Confirm bool
	Out     io.Writer
}

// vllmContext is everything the three commands share: resolved config, the
// spec they converge to, and the target address. repoRoot is "" when it
// could not be found; only apply's dirty-check and commit lookup need it.
type vllmContext struct {
	cfg      *types.Config
	host     string
	spec     vllm.Spec
	specPath string
	repoRoot string
}

// loadVLLMContext resolves tfvars, the target host and serving.yaml. Both
// reads are read-only: nothing here touches the GPU host or git.
func (app *App) loadVLLMContext(opts VLLMOptions) (*vllmContext, *vllm.Failure) {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		return nil, &vllm.Failure{
			Code: "tfvars_not_found",
			Msg:  fmt.Sprintf("could not locate terraform.tfvars: %v", err),
		}
	}
	if err := stateMgr.LoadTerraformExtras(context.Background()); err != nil {
		return nil, &vllm.Failure{
			Code: "tfvars_unreadable",
			Msg:  fmt.Sprintf("could not read %s: %v", cfg.TerraformTFVars, err),
		}
	}

	host := opts.Host
	if host == "" && cfg.GPUVMIP != nil {
		host = cfg.GPUVMIP.String()
	}
	if host == "" {
		return nil, &vllm.Failure{
			Code: "vllm_host_unset",
			Msg:  "no GPU VM address: gpu_vm_ip is absent from tfvars and --host was not given",
		}
	}

	// Resolved even when --spec was given explicitly: apply's dirty-check and
	// commit lookup need a directory to run git in, not just a file path.
	repoRoot, rootErr := findRepoRoot()
	if repoRoot != "" {
		// Best-effort: findRepoRoot just confirmed this directory exists, so
		// EvalSymlinks failing here would be a race, not a real condition to
		// report — fall back to the unresolved path rather than invent a
		// new failure mode for status and plan, which use repoRoot far more
		// loosely than apply's git checks do.
		if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
			repoRoot = resolved
		}
	}

	specPath := opts.Spec
	if specPath == "" {
		if rootErr != nil {
			return nil, &vllm.Failure{
				Code: "spec_unreadable",
				Msg:  fmt.Sprintf("could not resolve the repo root to find %s: %v", DefaultSpecPath, rootErr),
			}
		}
		specPath = filepath.Join(repoRoot, filepath.FromSlash(DefaultSpecPath))
	}

	// Resolved to an absolute path immediately, before Load and before any
	// git command: apply's git checks below run with Dir=repoRoot, so a
	// relative pathspec there is read relative to repoRoot, not to this
	// process's cwd. Loading a relative path with one meaning and then
	// git-checking it with another let an uncommitted spec pass the dirty
	// check silently — the two checks must agree on exactly which file.
	absSpecPath, absErr := filepath.Abs(specPath)
	if absErr != nil {
		return nil, &vllm.Failure{
			Code: "spec_unreadable",
			Msg:  fmt.Sprintf("could not resolve %s to an absolute path: %v", specPath, absErr),
		}
	}
	specPath = absSpecPath

	// git only ever sees a tracked symlink's target string, never what it
	// points at, so a spec that is itself a symlink to content outside the
	// repo would let an edit to that outside file take effect — Load
	// follows the symlink and reads it — while every check below still
	// reads the symlink's own (in-repo, tracked, clean) path. Resolving to
	// where the content actually lives, before Load and before any git
	// command, is what lets the repo-boundary check below catch that case.
	// Best-effort: a spec that does not exist yet fails in vllm.Load below
	// with its own, more specific error, not here.
	if resolved, err := filepath.EvalSymlinks(specPath); err == nil {
		specPath = resolved
	}

	spec, err := vllm.Load(specPath)
	if err != nil {
		return nil, &vllm.Failure{
			Code: "spec_unreadable",
			Msg:  fmt.Sprintf("could not read %s: %v", specPath, err),
		}
	}

	return &vllmContext{cfg: cfg, host: host, spec: spec, specPath: specPath, repoRoot: repoRoot}, nil
}

// findRepoRoot walks up from the working directory to the directory holding
// terraform/, the same anchor the rest of talops assumes tfvars and the
// vault sit next to — done independently of AnchorToRepoRoot's git-based
// chdir so this resolves correctly in a test working directory that has no
// .git at all, only a terraform/ marker.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := dir
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "terraform")); statErr == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no terraform/ directory found walking up from %s", start)
		}
		dir = parent
	}
}

// vllmClient builds the SSH client for the resolved host.
func (app *App) vllmClient(cfg *types.Config, host string) (*sshutil.Client, *vllm.Failure) {
	loginUser := cfg.GPUVMUser
	if loginUser == "" {
		loginUser = "vllm" // matches LoadTerraformExtras' default; belt and suspenders if tfvars was never loaded
	}
	client := sshutil.NewClient(loginUser, host, cfg.InsecureSSH)

	keyPath := cfg.GPUVMSSHKeyPath
	if keyPath == "" {
		keyPath = cfg.ProxmoxSSHKeyPath
	}

	if keyPath != "" {
		if err := client.UseKey(keyPath); err != nil {
			app.Logger.Warn("failed to load SSH key for the vLLM host, trying SSH agent",
				zap.String("key_path", keyPath), zap.Error(err))
			if !client.UseAgent() {
				return nil, sshAuthUnconfigured()
			}
		}
		return client, nil
	}

	if client.UseAgent() {
		return client, nil
	}
	return nil, sshAuthUnconfigured()
}

func sshAuthUnconfigured() *vllm.Failure {
	return &vllm.Failure{
		Code: "ssh_auth_unconfigured",
		Msg:  "no SSH auth available: set gpu_vm_ssh_key_path in the vaulted tfvars, pass --ssh-key, or run an SSH agent",
	}
}

// vllmGate builds the health gate for the port serving.yaml declares.
// Production sets both Target.Gate and Target.Models to it: Status needs
// Models to read the live served name, and without it that column would
// only ever say unknown.
func vllmGate(host string, spec vllm.Spec) vllm.HealthGate {
	return vllm.HealthGate{BaseURL: fmt.Sprintf("http://%s:%d", host, spec.Port)}
}

func (app *App) vllmOut(opts VLLMOptions) io.Writer {
	if opts.Out != nil {
		return opts.Out
	}
	return io.Discard
}

// emitVLLMJSON writes one newline-delimited JSON object tagged with the
// event name, the same shape haproxy.EmitJSON produces. Kept local so this
// command group does not import haproxy's types for an unrelated feature.
func emitVLLMJSON(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	name, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	fields["event"] = name
	out, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	if _, err := fmt.Fprintln(w, string(out)); err != nil {
		return fmt.Errorf("write %s event: %w", event, err)
	}
	return nil
}

// RunVLLMStatus compares serving.yaml, the applied record and the running
// container. It reads only: nothing here writes to the GPU host or git.
func (app *App) RunVLLMStatus(ctx context.Context, opts VLLMOptions) error {
	res := vllm.StatusResult{Host: opts.Host}

	vctx, failure := app.loadVLLMContext(opts)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		return app.emitVLLMStatus(opts, res)
	}
	res.Host = vctx.host

	client, failure := app.vllmClient(vctx.cfg, vctx.host)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		return app.emitVLLMStatus(opts, res)
	}

	gate := vllmGate(vctx.host, vctx.spec)
	target := vllm.Target{Host: vctx.host, Runner: client, Gate: gate, Models: gate}

	return app.emitVLLMStatus(opts, vllm.Status(ctx, target, vctx.spec))
}

// errVLLMDrift is status's own exit-code contract, so a script can gate on
// drift without parsing the report. The report already carries the reason
// (drift: true and the fields that disagree), so this error is never
// printed on its own — it only has to be non-nil.
var errVLLMDrift = errors.New("vllm status: drift against serving.yaml")

// emitVLLMStatus prints the report and maps the outcome to the caller's exit
// code: a Failure, or drift with no Failure at all. Status is the one
// command where "nothing failed, but it doesn't match" still has to fail
// the exit code, since nothing else reads the report body for that signal.
func (app *App) emitVLLMStatus(opts VLLMOptions, res vllm.StatusResult) error {
	if opts.JSON {
		if err := emitVLLMJSON(app.vllmOut(opts), "status", res); err != nil {
			return err
		}
	} else if _, err := fmt.Fprint(app.vllmOut(opts), vllm.ReportStatus(res)); err != nil {
		return err
	}
	if res.Failure != nil {
		return res.Failure
	}
	if res.Drift {
		return errVLLMDrift
	}
	return nil
}

// RunVLLMPlan shows what apply would change and who it interrupts. It
// mutates nothing, and drift is the answer, not a failure: this exits
// successfully either way, and changed: true|false is the signal to branch on.
func (app *App) RunVLLMPlan(ctx context.Context, opts VLLMOptions) error {
	res := vllm.PlanResult{Host: opts.Host}

	vctx, failure := app.loadVLLMContext(opts)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		return app.emitVLLMPlan(opts, res)
	}
	res.Host = vctx.host

	client, failure := app.vllmClient(vctx.cfg, vctx.host)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		return app.emitVLLMPlan(opts, res)
	}

	gate := vllmGate(vctx.host, vctx.spec)
	target := vllm.Target{Host: vctx.host, Runner: client, Gate: gate, Models: gate}

	return app.emitVLLMPlan(opts, vllm.Plan(ctx, target, vctx.spec))
}

func (app *App) emitVLLMPlan(opts VLLMOptions, res vllm.PlanResult) error {
	if opts.JSON {
		if err := emitVLLMJSON(app.vllmOut(opts), "plan", res); err != nil {
			return err
		}
	} else if _, err := fmt.Fprint(app.vllmOut(opts), vllm.ReportPlan(res)); err != nil {
		return err
	}
	if res.Failure != nil {
		return res.Failure
	}
	return nil
}

// RunVLLMApply converges the GPU host to serving.yaml: SSH in, install the
// Quadlet under hostconverge's validated, auto-rollback path, gate on
// health, and record what was applied.
func (app *App) RunVLLMApply(ctx context.Context, opts VLLMOptions) error {
	res := vllm.ApplyResult{Host: opts.Host}

	// Checked before anything else is resolved: a missing --confirm is a
	// usage problem, not a failure to reach tfvars, the spec or the host.
	if !opts.Confirm {
		res.Failure = &vllm.Failure{
			Code: CodeConfirmRequired,
			Msg:  "apply requires --confirm: it restarts the vLLM server and interrupts every consumer until the health gate passes",
		}
		res.Help = helpForVLLMFailure(res.Failure)
		return app.emitVLLMApply(opts, res)
	}

	vctx, failure := app.loadVLLMContext(opts)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		return app.emitVLLMApply(opts, res)
	}
	res.Host = vctx.host

	if vctx.repoRoot == "" {
		res.Failure = &vllm.Failure{
			Code: "repo_root_unresolved",
			Msg:  fmt.Sprintf("could not find the repo's terraform/ directory to check %s against git", vctx.specPath),
		}
		res.Help = []string{"run talops from within the infrastructure checkout, or pass a --spec path under it: " + vctx.specPath}
		return app.emitVLLMApply(opts, res)
	}

	// specRel is the same file as vctx.specPath, expressed relative to
	// repoRoot: every git command below runs with Dir=repoRoot, and a
	// pathspec has to be given in terms of that directory, not the
	// process's cwd, or a relative --spec would name a different file to
	// git than the one Load just read.
	specRel, err := filepath.Rel(vctx.repoRoot, vctx.specPath)
	if err != nil || specRel == ".." || strings.HasPrefix(specRel, ".."+string(filepath.Separator)) {
		res.Failure = &vllm.Failure{
			Code: "spec_outside_repo",
			Msg:  fmt.Sprintf("%s does not lie under the repo root %s: an apply must be checked against git", vctx.specPath, vctx.repoRoot),
		}
		res.Help = []string{"point --spec at a file under " + vctx.repoRoot + " (got " + vctx.specPath + ")"}
		return app.emitVLLMApply(opts, res)
	}

	if _, err := runGit(vctx.repoRoot, "ls-files", "--error-unmatch", "--", specRel); err != nil {
		res.Failure = &vllm.Failure{
			Code: "spec_untracked",
			Msg:  fmt.Sprintf("%s is not tracked by git: an apply must correspond to a commit", vctx.specPath),
		}
		res.Help = []string{"git add " + vctx.specPath + " && git commit, then re-run talops vllm apply --confirm"}
		return app.emitVLLMApply(opts, res)
	}

	dirty, err := gitPorcelainStatus(vctx.repoRoot, specRel)
	if err != nil {
		res.Failure = &vllm.Failure{
			Code: "dirty_spec_unknown",
			Msg:  fmt.Sprintf("could not read git status of %s: %v", vctx.specPath, err),
		}
		res.Help = []string{"check git works in " + vctx.repoRoot + " for " + vctx.specPath + ", then re-run talops vllm apply --confirm"}
		return app.emitVLLMApply(opts, res)
	}
	if strings.TrimSpace(dirty) != "" {
		res.Failure = &vllm.Failure{
			Code: "dirty_spec",
			Msg:  fmt.Sprintf("%s has uncommitted changes: an apply must correspond to a commit", vctx.specPath),
		}
		res.Help = []string{"commit " + vctx.specPath + ", then re-run talops vllm apply --confirm"}
		return app.emitVLLMApply(opts, res)
	}

	commit, err := gitRevParseHead(vctx.repoRoot)
	if err != nil {
		res.Failure = &vllm.Failure{
			Code: "commit_unreadable",
			Msg:  fmt.Sprintf("could not resolve git HEAD in %s to record against %s: %v", vctx.repoRoot, vctx.specPath, err),
		}
		res.Help = []string{"check git works in " + vctx.repoRoot + " for " + vctx.specPath + ", then re-run talops vllm apply --confirm"}
		return app.emitVLLMApply(opts, res)
	}

	notes := unmergedCommitNotes(vctx.repoRoot)

	client, failure := app.vllmClient(vctx.cfg, vctx.host)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForVLLMFailure(failure)
		res.Notes = notes
		return app.emitVLLMApply(opts, res)
	}

	gate := vllmGate(vctx.host, vctx.spec)
	target := vllm.Target{
		Host:   vctx.host,
		Runner: client,
		Gate:   gate,
		Models: gate,
		Commit: commit,
		By:     operatorIdentity(),
	}

	res = vllm.Apply(ctx, target, vctx.spec)
	res.Notes = append(notes, res.Notes...)
	return app.emitVLLMApply(opts, res)
}

// unmergedCommitNotes says when the commit being recorded is not on
// origin/main. Applying a branch is legitimate (testing a change before it
// merges), so this never refuses; but the record then names a commit that
// main may never contain, and a later apply from main undoes the change.
// origin/main is read as the checkout last fetched it, with no fetch here.
func unmergedCommitNotes(repoRoot string) []string {
	if _, err := runGit(repoRoot, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); err != nil {
		return []string{"cannot tell whether HEAD is merged: this checkout has no origin/main; the applied record names a commit main may not contain"}
	}
	if _, err := runGit(repoRoot, "merge-base", "--is-ancestor", "HEAD", "refs/remotes/origin/main"); err != nil {
		return []string{"HEAD is not on origin/main (as last fetched): the applied record names an unmerged commit, and a later apply from main undoes whatever it changed"}
	}
	return nil
}

func (app *App) emitVLLMApply(opts VLLMOptions, res vllm.ApplyResult) error {
	if opts.JSON {
		if err := emitVLLMJSON(app.vllmOut(opts), "apply", res); err != nil {
			return err
		}
	} else if _, err := fmt.Fprint(app.vllmOut(opts), vllm.ReportApply(res)); err != nil {
		return err
	}
	if res.Failure != nil {
		return res.Failure
	}
	return nil
}

// operatorIdentity names who ran the apply, for the applied record: $USER
// (falling back to the OS-reported login name) @ the hostname it ran from.
func operatorIdentity() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		name = "unknown"
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return name + "@" + host
}

func gitRevParseHead(repoRoot string) (string, error) {
	out, err := runGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func gitPorcelainStatus(repoRoot, path string) (string, error) {
	return runGit(repoRoot, "status", "--porcelain", "--", path)
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// helpForVLLMFailure names the command that moves the caller forward from
// the state just reported, the same shape helpForFailure uses for haproxy.
// Apply's git-path-aware failures (repo_root_unresolved, spec_outside_repo,
// spec_untracked, dirty_spec, dirty_spec_unknown, commit_unreadable) build
// their own Help inline instead of going through this generic, code-only
// switch — each of them has to name the actual resolved spec path, which
// this function has no way to see.
func helpForVLLMFailure(f *vllm.Failure) []string {
	if f == nil {
		return nil
	}
	switch f.Code {
	case "vllm_host_unset":
		return []string{"pass --host 192.168.1.50 or add gpu_vm_ip to the vaulted tfvars (talops secrets edit)"}
	case "tfvars_not_found", "tfvars_unreadable":
		return []string{
			"Check the vault is hydrated: talops secrets status",
			"Point at the file explicitly: talops --tfvars <path> vllm status",
		}
	case "spec_unreadable":
		return []string{
			"expected " + DefaultSpecPath + " at the repo root",
			"write it, or point elsewhere: talops vllm status --spec <path>",
		}
	case CodeConfirmRequired:
		return []string{"talops vllm apply --confirm"}
	case "ssh_auth_unconfigured":
		return []string{
			"set gpu_vm_ssh_key_path in the vaulted tfvars, or pass --ssh-key",
			"or run an SSH agent with the GPU host's key loaded",
		}
	}
	return nil
}
