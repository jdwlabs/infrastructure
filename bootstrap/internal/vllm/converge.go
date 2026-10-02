package vllm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/hostconverge"
)

// Host paths apply writes and plan reads. The Quadlet's file name sets its
// generated unit's name, vllm-server.service: it must differ from the legacy
// venv unit's vllm.service, because a unit file in /etc/systemd/system
// shadows a generated one of the same name and would keep the legacy
// server running under the new unit's name.
const (
	QuadletPath = "/etc/containers/systemd/vllm-server.container"
	AppliedPath = "/etc/vllm/applied.json"
)

// Stable failure codes: the CLI maps them to exit codes and the docs name them.
const (
	CodeHostPrereq      = "host_prereq_failed"
	CodeStage           = "stage_failed"
	CodeCDIUnresolvable = "cdi_unresolvable"
	CodePreflight       = "preflight_failed"
	CodeConverge        = "converge_failed"
	CodeGate            = "gate_failed"
	CodeRecord          = "record_failed"
	CodeLegacyUnknown   = "legacy_state_unknown"
	CodeRead            = "read_failed"
	CodeConfigInvalid   = "config_invalid"
)

// Serving values on an ApplyResult: which server is answering after apply.
const (
	ServingNew      = "new"
	ServingPrevious = "previous"
	ServingNone     = "none"
)

// Status column values that are not observations of the thing itself.
const (
	colAbsent      = "(none)"
	colNotRunning  = "(not running)"
	colUnreachable = "unreachable"
	colUnknown     = "unknown"
	colNotVisible  = "n/a"
)

const runbook = "scenarios/ai-sre-agent-runbook.md"

// Consumers are the services an apply interrupts while the server restarts.
var Consumers = []string{
	"@server answers (jdw-deployments minecraft-fwb agent.llm)",
	"LiteLLM sre-investigator-local (platform litellm)",
}

// Gatekeeper decides whether a server is serving the spec. Wait gates a
// server apply just started and must bound itself by s.HealthGate.Timeout;
// Check is one attempt, for a server apply left alone and so has no reason
// to wait for.
type Gatekeeper interface {
	Wait(ctx context.Context, s Spec) error
	Check(ctx context.Context, s Spec) error
}

// ModelsReader lists what the server currently serves, in one request.
// Status reads it rather than the Gatekeeper so a status never blocks for
// a gate's full timeout on a server that is down.
type ModelsReader interface {
	Models(ctx context.Context) ([]ModelEntry, error)
}

// Target is the GPU host and everything apply needs to act on it and to
// attribute what it wrote. HealthGate satisfies both Gate and Models.
type Target struct {
	Host   string
	Runner hostconverge.Runner
	Gate   Gatekeeper
	Models ModelsReader
	Commit string // git HEAD of the repo serving.yaml came from
	By     string // operator identity: $USER@hostname
	Now    func() time.Time
}

func (t Target) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Failure is a machine-readable reason a command could not finish, paired
// with prose the caller can act on.
type Failure struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (f *Failure) Error() string { return f.Msg }

// Field is one row of the three-way comparison status prints.
type Field struct {
	Name    string `json:"name"`
	Git     string `json:"git"`
	Applied string `json:"applied"`
	Live    string `json:"live"`
}

// Phase is how long one step of apply took, so the outage window is measured
// on every run instead of estimated.
type Phase struct {
	Name string        `json:"name"`
	Took time.Duration `json:"tookNs"`
}

type StatusResult struct {
	Host    string   `json:"host"`
	Fields  []Field  `json:"fields"`
	Drift   bool     `json:"drift"`
	Legacy  bool     `json:"legacy"`
	Notes   []string `json:"notes,omitempty"`
	Failure *Failure `json:"error,omitempty"`
	Help    []string `json:"-"`
}

type PlanResult struct {
	Host      string   `json:"host"`
	Changed   bool     `json:"changed"`
	Reasons   []string `json:"reasons,omitempty"`
	Diff      string   `json:"diff,omitempty"`
	Consumers []string `json:"consumers,omitempty"`
	Failure   *Failure `json:"error,omitempty"`
	Help      []string `json:"-"`
}

type ApplyResult struct {
	Host       string   `json:"host"`
	Changed    bool     `json:"changed"`
	RolledBack bool     `json:"rolledBack"`
	Serving    string   `json:"serving"`
	Reasons    []string `json:"reasons,omitempty"`
	Phases     []Phase  `json:"phases"`
	Notes      []string `json:"notes,omitempty"`
	Failure    *Failure `json:"error,omitempty"`
	Help       []string `json:"-"`
}

func quadletFile(s Spec) hostconverge.File {
	return hostconverge.File{Path: QuadletPath, Content: []byte(Quadlet(s))}
}

// pendingReasons says why apply would act, one reason per thing that
// differs; empty means the host already matches s. Plan and Apply share it
// so a plan that says "no change" can never precede an apply that restarts.
// It also returns the applied record it read, nil when there is none.
func pendingReasons(ctx context.Context, r hostconverge.Runner, s Spec) ([]string, *Applied, *Failure) {
	var reasons []string

	same, err := hostconverge.Unchanged(ctx, r, []hostconverge.File{quadletFile(s)})
	if err != nil {
		return nil, nil, &Failure{Code: CodeRead, Msg: fmt.Sprintf("read installed unit %s: %v", QuadletPath, err)}
	}
	if !same {
		reasons = append(reasons, "unit "+QuadletPath+" differs from serving.yaml")
	}

	same, err = hostconverge.Unchanged(ctx, r, DriftFiles())
	if err != nil {
		return nil, nil, &Failure{Code: CodeRead, Msg: fmt.Sprintf("read installed drift check: %v", err)}
	}
	if !same {
		reasons = append(reasons, "drift check script or units differ from this talops build")
	}

	rec, err := ReadApplied(ctx, r)
	if err != nil {
		return nil, nil, &Failure{Code: CodeRead, Msg: err.Error()}
	}
	switch {
	case rec == nil:
		reasons = append(reasons, AppliedPath+" is absent: nothing has been applied")
	case !recordMatches(*rec, s):
		reasons = append(reasons, AppliedPath+" records a different image, model revision or args")
	}

	legacy, err := LegacyPresent(ctx, r)
	if err != nil {
		return nil, nil, &Failure{Code: CodeLegacyUnknown, Msg: err.Error()}
	}
	if legacy {
		reasons = append(reasons, "legacy vllm.service is active or enabled and is replaced by the Quadlet unit")
	}
	return reasons, rec, nil
}

// previousIdentity is what the rollback gate must find serving: the server
// the rollback restores answers under the name and model its own apply
// recorded, and gating it as s would fail a healthy server whenever the
// change being rolled back renamed or replaced the model. With no record
// there is no better statement of the previous server than s.
func previousIdentity(s Spec, prior *Applied) Spec {
	if prior == nil {
		return s
	}
	s.ServedName = prior.ServedName
	s.Model.Repo = prior.ModelRepo
	return s
}

func recordMatches(a Applied, s Spec) bool {
	return a.ArgsHash == ArgsHash(s) && a.ImageDigest == s.ImageDigest() && a.ModelRevision == s.Model.Revision
}

// Apply converges the host to s: host prerequisites, staging and a CDI
// preflight while the previous server keeps serving, then the unit swap
// under hostconverge's backup/verify/rollback, gated on the health check,
// and only then the applied record. It never returns an error; everything
// the caller needs, including which server is answering afterwards, is in
// the result.
func Apply(ctx context.Context, t Target, s Spec) ApplyResult {
	res := ApplyResult{Host: t.Host, Serving: ServingPrevious}
	r := t.Runner
	if t.Gate == nil {
		return failApply(res, CodeConfigInvalid, "no health gate configured: apply never swaps the server without one")
	}

	phase := func(name string, start time.Time) {
		res.Phases = append(res.Phases, Phase{Name: name, Took: t.now().Sub(start)})
	}

	start := t.now()
	hostChanges, err := EnsureHost(ctx, r)
	phase("host", start)
	for _, c := range hostChanges {
		res.Notes = append(res.Notes, "host: "+c)
	}
	if err != nil {
		return failApply(res, CodeHostPrereq, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return failApply(res, CodeStage, cancelledMsg(err))
	}

	reasons, prior, f := pendingReasons(ctx, r, s)
	if f != nil {
		return failApply(res, f.Code, f.Msg)
	}
	if len(reasons) == 0 {
		// Nothing to change is not proof of health: a crashed container
		// matching the spec would otherwise read as serving.
		start = t.now()
		err := t.Gate.Check(ctx, s)
		phase("gate", start)
		if err != nil {
			// An interrupted check is not evidence the server is down —
			// nothing was restarted here either way, so treat a cancel the
			// same as every other pre-swap interrupt rather than sending
			// the operator to the down-server runbook for a server that
			// was never examined.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failApply(res, CodeStage, cancelledMsg(ctxErr))
			}
			res.Serving = ServingNone
			res.Failure = &Failure{Code: CodeGate, Msg: err.Error()}
			res.Help = []string{
				"the vLLM endpoint is down: the host matches serving.yaml but the server fails its health check, and talops restarted nothing",
				"recover by hand with " + runbook,
				"talops vllm status  # see what the host is running",
			}
			return res
		}
		res.Serving = ServingNew
		return res
	}
	res.Reasons = reasons
	res.Changed = true

	start = t.now()
	err = Stage(ctx, r, s)
	phase("stage", start)
	if err != nil {
		return failApply(res, CodeStage, err.Error())
	}
	// A runner that ignores ctx, or a step that finished just as the
	// operator interrupted, still returns success; the swap is the one step
	// that interrupts consumers, so an interrupt must never reach it.
	if err := ctx.Err(); err != nil {
		return failApply(res, CodeStage, cancelledMsg(err))
	}

	start = t.now()
	err = PreflightCDI(ctx, r, s)
	phase("cdi-preflight", start)
	if err != nil {
		// A preflight killed by an interrupt says nothing about the CDI
		// spec, so it is reported as the interrupt.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failApply(res, CodeStage, cancelledMsg(ctxErr))
		}
		if errors.Is(err, ErrCDIUnresolvable) {
			return failApply(res, CodeCDIUnresolvable, err.Error())
		}
		return failApply(res, CodePreflight, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return failApply(res, CodeStage, cancelledMsg(err))
	}

	// Read again after staging rather than reusing pendingReasons' answer:
	// staging can take many minutes, and the swap must act on the state the
	// legacy unit is in now.
	legacy, err := ReadLegacy(ctx, r)
	if err != nil {
		return failApply(res, CodeLegacyUnknown, err.Error())
	}

	// hostconverge calls Activate then Verify forward, and both again after
	// a rollback; a forward Activate that fails skips straight to the
	// rollback pair. Counting activations is what tells the two gates
	// apart, so a failing rollback gate is never reported as the new
	// server failing its gate, and what says which rollback step failed.
	activations := 0
	activateUnit := activate(s, legacy)
	restored := previousIdentity(s, prior)
	var gates []Phase
	var forwardGateErr, rollbackActivateErr, rollbackGateErr error
	verify := func(ctx context.Context, _ hostconverge.Runner) error {
		name, want := "gate", s
		if activations > 1 {
			name, want = "gate-rollback", restored
		}
		vStart := t.now()
		err := t.Gate.Wait(ctx, want)
		gates = append(gates, Phase{Name: name, Took: t.now().Sub(vStart)})
		if name == "gate" {
			forwardGateErr = err
		} else {
			rollbackGateErr = err
		}
		return err
	}

	files := append([]hostconverge.File{quadletFile(s)}, DriftFiles()...)
	start = t.now()
	cres, err := hostconverge.Apply(ctx, r, hostconverge.Change{
		Files: files,
		Activate: func(ctx context.Context, r hostconverge.Runner) error {
			activations++
			err := activateUnit(ctx, r)
			if activations > 1 {
				rollbackActivateErr = err
			}
			return err
		},
		Verify: verify,
	})
	converge := Phase{Name: "converge", Took: t.now().Sub(start)}
	for _, g := range gates {
		converge.Took -= g.Took
	}
	res.Phases = append(append(res.Phases, converge), gates...)
	res.RolledBack = cres.RolledBack

	if err != nil {
		code := CodeConverge
		if forwardGateErr != nil {
			code = CodeGate
		}
		if cres.RollbackErr == nil {
			return failApply(res, code, err.Error())
		}
		res.Serving = ServingNone
		res = failApply(res, code, err.Error())
		res.Help = downHelp(rollbackActivateErr, rollbackGateErr)
		return res
	}
	res.Serving = ServingNew

	start = t.now()
	record := NewApplied(s, t.Commit, t.By, t.now())
	_, err = hostconverge.Apply(ctx, r, hostconverge.Change{Files: []hostconverge.File{
		{Path: AppliedPath, Content: record.JSON(), Mode: 0o644},
	}})
	phase("record", start)
	if err != nil {
		return failApply(res, CodeRecord, fmt.Sprintf("write %s: %v", AppliedPath, err))
	}

	// The server is up and gated; a drift check that will not start only
	// delays the first metric by one timer interval, so it is not a failure.
	if _, err := r.Run(ctx, "sudo systemctl start vllm-drift-check.service"); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("drift check did not run once after apply (the timer runs it next): %v", err))
	}
	return res
}

func cancelledMsg(err error) string {
	return fmt.Sprintf("cancelled before the swap; nothing on the running server changed: %v", err)
}

// Rollback outcomes that leave the host without a server, each named so the
// help can say which recovery applies.
var (
	// Nothing was serving before this first apply (no legacy unit, or one
	// that was enabled but stopped), so there is no server to bring back,
	// and a rollback gate against nothing would only wait out its timeout.
	errNoPreviousServer = errors.New("no previous server to restore: nothing was serving before the first apply")

	// The rejected server is still loaded and would not stop, so nothing
	// else may start: it may still hold port 8000 and the GPU.
	errNewServerWillNotStop = errors.New("the new server would not stop")
)

const (
	timerWantsLink = "/etc/systemd/system/timers.target.wants/vllm-drift-check.timer"

	// driftMetrics is the textfile the drift check writes. A rolled-back
	// first apply removes the check itself, so nothing would ever refresh
	// or remove this file again, and node-exporter would keep exporting
	// the rejected spec's info and drift series.
	driftMetrics = "/var/lib/prometheus/node-exporter/vllm_serving.prom"
)

// activate is the Change.Activate hook for the unit swap, and hostconverge
// calls it again after a rollback restores the previous files. Which server
// to start is read from the Quadlet file on the host, not remembered, with
// four outcomes:
//
//   - present, new content: the forward swap.
//   - present, old content: a rollback restored the previous Quadlet.
//   - absent, legacy present at start: a rollback of the first apply; the
//     new server is stopped before the legacy unit is touched, since both
//     bind the same port and GPU, and the legacy unit gets back exactly the
//     enabled and running state it had.
//   - absent, no legacy: a rollback of the first apply on a host that
//     served nothing; the new server is stopped and the rollback fails,
//     because there is no previous server to report as serving.
//
// Any read that cannot say which case this is returns its error; guessing
// here starts the wrong server or restarts one whose unit file is gone.
func activate(s Spec, legacy LegacyState) func(context.Context, hostconverge.Runner) error {
	return func(ctx context.Context, r hostconverge.Runner) error {
		probe, err := r.Run(ctx, "sudo sh -c 'test -e "+QuadletPath+" || echo absent'")
		if err != nil {
			return fmt.Errorf("check installed unit %s: %w", QuadletPath, err)
		}
		if strings.Contains(probe, "absent") {
			return retireNewServer(ctx, r, legacy)
		}

		isNew, err := hostconverge.Unchanged(ctx, r, []hostconverge.File{quadletFile(s)})
		if err != nil {
			return fmt.Errorf("read installed unit %s: %w", QuadletPath, err)
		}

		var cmd string
		if isNew {
			cmd = "sudo systemctl daemon-reload && "
			if legacy.Present() {
				cmd += "sudo systemctl disable --now vllm.service && "
			}
			cmd += "sudo systemctl restart vllm-server && sudo systemctl enable --now vllm-drift-check.timer"
		} else {
			// The timer's enablement was never undone by the rollback, so
			// only the server needs restarting.
			cmd = "sudo systemctl daemon-reload && sudo systemctl restart vllm-server"
		}
		if _, err := r.Run(ctx, cmd); err != nil {
			return fmt.Errorf("activate: %w", err)
		}
		return nil
	}
}

// retireNewServer undoes a first apply's units after the rollback removed
// their files, then puts the legacy unit back as it was. The daemon-reload
// comes after the stops: until then the removed units are still loaded, and
// stop is what needs them loaded.
func retireNewServer(ctx context.Context, r hostconverge.Runner, legacy LegacyState) error {
	if _, err := r.Run(ctx, "sudo systemctl stop vllm-server"); err != nil {
		// A forward activation that failed before its daemon-reload never
		// loaded the unit, so there is nothing to stop; anything else means
		// the new server may still hold the port and the GPU.
		state, sErr := r.Run(ctx, "systemctl show -p LoadState --value vllm-server 2>/dev/null")
		if sErr != nil || strings.TrimSpace(state) != "not-found" {
			return fmt.Errorf("%w: %v", errNewServerWillNotStop, err)
		}
	}

	// systemctl disable reads the unit file, which the rollback has already
	// removed, so the timer is stopped and its enablement link removed
	// directly. All three are tolerated: what they leave behind is a check
	// that reports not_running or a stale metric, and neither may stand
	// between the host and its previous server.
	_, _ = r.Run(ctx, "sudo systemctl stop vllm-drift-check.timer")
	_, _ = r.Run(ctx, "sudo rm -f "+timerWantsLink)
	_, _ = r.Run(ctx, "sudo rm -f "+driftMetrics)

	if _, err := r.Run(ctx, "sudo systemctl daemon-reload"); err != nil {
		return fmt.Errorf("reload units after removing vllm-server: %w", err)
	}

	var cmd string
	switch {
	case legacy.Enabled && legacy.Active:
		cmd = "sudo systemctl enable --now vllm.service"
	case legacy.Enabled:
		cmd = "sudo systemctl enable vllm.service"
	case legacy.Active:
		cmd = "sudo systemctl start vllm.service"
	default:
		return errNoPreviousServer
	}
	if _, err := r.Run(ctx, cmd); err != nil {
		return fmt.Errorf("restore legacy vllm.service: %w", err)
	}
	if !legacy.Active {
		return errNoPreviousServer
	}
	return nil
}

// downHelp says why a rollback left no server, one line per cause, since
// each needs a different recovery.
func downHelp(rollbackActivateErr, rollbackGateErr error) []string {
	var why string
	switch {
	case errors.Is(rollbackActivateErr, errNewServerWillNotStop):
		return []string{
			"the vLLM endpoint is down: the new server would not stop; stop vllm-server by hand before starting anything else, see " + runbook,
			"talops vllm status  # see what the host is running",
		}
	case errors.Is(rollbackActivateErr, errNoPreviousServer):
		why = "the vLLM endpoint is down: nothing was serving before this first apply, and the new server failed, so it was stopped and removed"
	case rollbackActivateErr != nil:
		why = "the vLLM endpoint is down: rollback restored the previous files but could not start the previous server"
	case rollbackGateErr != nil:
		why = "the vLLM endpoint is down: rollback restarted the previous server but it fails the health gate too"
	default:
		why = "the vLLM endpoint is down: rollback could not restore the previous files, so the host may hold a mix of new and previous units"
	}
	return []string{
		why,
		"recover by hand with " + runbook,
		"talops vllm status  # see what the host is running",
	}
}

func failApply(res ApplyResult, code, msg string) ApplyResult {
	res.Failure = &Failure{Code: code, Msg: msg}
	switch {
	case code == CodeRecord:
		res.Help = []string{
			"the new server is serving and passed the health gate, but " + AppliedPath + " was not written, so status and the drift check report drift",
			"talops vllm apply --confirm  # rewrite the record once the cause is fixed",
		}
	case res.RolledBack:
		res.Help = []string{
			"the previous server is serving again and passed the health gate",
			"talops vllm status  # compare what serves against serving.yaml",
		}
	case code == CodeCDIUnresolvable:
		res.Help = []string{
			"nothing changed on the running server: podman could not give the new image the GPU, so the swap was never attempted",
			"see docs/vllm-serving.md#troubleshooting (unresolvable CDI devices)",
			"talops vllm apply --confirm  # re-run once the cause is fixed",
		}
	case code == CodePreflight:
		res.Help = []string{
			"nothing changed on the running server: the GPU could not be verified for the new image, so the swap was never attempted",
			"the failure message carries the underlying error; podman did not report a CDI problem",
			"talops vllm apply --confirm  # re-run once the cause is fixed",
		}
	default:
		res.Help = []string{
			"nothing changed on the running server",
			"talops vllm apply --confirm  # re-run once the cause is fixed",
		}
	}
	return res
}

// Status compares serving.yaml (git), the applied record and the running
// container. It mutates nothing and keeps going past a failed read, so one
// unreadable source still leaves the other columns to look at.
func Status(ctx context.Context, t Target, s Spec) StatusResult {
	res := StatusResult{Host: t.Host}
	r := t.Runner
	fail := func(code, msg string) {
		if res.Failure == nil {
			res.Failure = &Failure{Code: code, Msg: msg}
			return
		}
		res.Notes = append(res.Notes, code+": "+msg)
	}

	rec, recErr := ReadApplied(ctx, r)
	if recErr != nil {
		fail(CodeRead, recErr.Error())
	}
	live, liveErr := ReadLive(ctx, r)
	if liveErr != nil {
		fail(CodeRead, liveErr.Error())
	}
	legacy, legErr := LegacyPresent(ctx, r)
	if legErr != nil {
		fail(CodeLegacyUnknown, legErr.Error())
	}
	res.Legacy = legacy

	applied := func(get func(Applied) string) string {
		switch {
		case recErr != nil:
			return colUnknown
		case rec == nil:
			return colAbsent
		default:
			return get(*rec)
		}
	}
	liveOr := func(v string) string {
		switch {
		case liveErr != nil:
			return colUnknown
		case !live.Running:
			return colNotRunning
		default:
			return v
		}
	}

	var recDigest, recServed string
	if rec != nil {
		recDigest, recServed = rec.ImageDigest, rec.ServedName
	}

	res.Fields = []Field{
		{
			Name:    "imageDigest",
			Git:     s.ImageDigest(),
			Applied: applied(func(a Applied) string { return a.ImageDigest }),
			Live:    liveOr(liveDigest(live, s.ImageDigest(), recDigest)),
		},
		{
			Name:    "modelRevision",
			Git:     s.Model.Revision,
			Applied: applied(func(a Applied) string { return a.ModelRevision }),
			// podman sees the container's argv, and the revision is in it
			// only as part of the args hash; reading it back from the model
			// cache would report what is downloaded, not what is loaded.
			Live: colNotVisible,
		},
		{
			Name:    "servedName",
			Git:     s.ServedName,
			Applied: applied(func(a Applied) string { return a.ServedName }),
			Live:    liveServedName(ctx, t.Models, s.ServedName, recServed),
		},
		{
			Name:    "argsHash",
			Git:     ArgsHash(s),
			Applied: applied(func(a Applied) string { return a.ArgsHash }),
			Live:    liveOr(live.ArgsHash),
		},
	}

	for _, f := range res.Fields {
		if f.Applied != f.Git || (f.Live != colNotVisible && f.Live != f.Git) {
			res.Drift = true
		}
	}
	if res.Legacy {
		res.Drift = true
		res.Notes = append(res.Notes, "legacy vllm.service is active or enabled: the first apply replaces it with the Quadlet unit")
	}

	if res.Drift {
		res.Help = []string{
			"talops vllm plan  # see what apply would change and who it interrupts",
			"talops vllm apply --confirm  # converge the host to serving.yaml",
		}
	}
	return res
}

// liveDigest names the running image by the pinned digest it was pulled as.
// RepoDigests holds per-arch instance references, so the index digests from
// git and the record are tested for membership; a running image matching
// neither is shown by its own first reference so the operator sees what it is.
func liveDigest(l Live, candidates ...string) string {
	for _, d := range candidates {
		if d != "" && l.HasDigest(d) {
			return d
		}
	}
	if len(l.RepoDigests) > 0 {
		return l.RepoDigests[0]
	}
	return "(no repo digest)"
}

func liveServedName(ctx context.Context, m ModelsReader, candidates ...string) string {
	if m == nil {
		return colUnknown
	}
	entries, err := m.Models(ctx)
	if err != nil {
		return colUnreachable
	}
	for _, want := range candidates {
		for _, e := range entries {
			if want != "" && e.ID == want {
				return e.ID
			}
		}
	}
	if len(entries) > 0 {
		return entries[0].ID
	}
	return colAbsent
}

// Plan shows the unit diff and every other reason apply would act, and who
// an apply would interrupt. It mutates nothing.
func Plan(ctx context.Context, t Target, s Spec) PlanResult {
	res := PlanResult{Host: t.Host}

	deployed, err := readDeployedUnit(ctx, t.Runner)
	if err != nil {
		res.Failure = &Failure{Code: CodeRead, Msg: err.Error()}
		return res
	}
	res.Diff = lineDiff(deployed, Quadlet(s), "deployed "+QuadletPath, "rendered")

	reasons, _, f := pendingReasons(ctx, t.Runner, s)
	if f != nil {
		res.Failure = f
		return res
	}
	res.Reasons = reasons
	res.Changed = len(reasons) > 0
	if res.Changed {
		res.Consumers = Consumers
		res.Help = []string{"talops vllm apply --confirm  # restarts the server; the consumers above lose it until the health gate passes"}
	}
	return res
}

// readDeployedUnit returns the installed Quadlet, or "" when none is
// installed. Existence is probed separately so a failed read is an error
// rather than an empty unit that diffs as "everything added".
func readDeployedUnit(ctx context.Context, r hostconverge.Runner) (string, error) {
	probe, err := r.Run(ctx, "sudo sh -c 'test -e "+QuadletPath+" || echo absent'")
	if err != nil {
		return "", fmt.Errorf("check installed unit %s: %w", QuadletPath, err)
	}
	if strings.Contains(probe, "absent") {
		return "", nil
	}
	out, err := r.Run(ctx, "sudo cat "+QuadletPath+" 2>/dev/null")
	if err != nil {
		return "", fmt.Errorf("read installed unit %s: %w", QuadletPath, err)
	}
	return out, nil
}
