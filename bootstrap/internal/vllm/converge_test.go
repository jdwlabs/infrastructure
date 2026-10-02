package vllm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/hostconverge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rule scripts one command: the first rule whose sub is contained in the
// command answers it, before the emulated filesystem is consulted. once
// rules answer a single time, so "fails once then passes" is expressible.
type rule struct {
	sub  string
	out  string
	err  error
	once bool
	used bool
}

// fakeHost emulates the filesystem commands hostconverge and remote.go
// issue (write, probe, backup, move, restore, remove, hash, cat, test,
// touch) and the systemctl verbs apply uses, so a rollback leaves real
// state behind that the next Activate reads back, and a test can assert
// what is running rather than only which strings were sent.
//
// systemctl follows the rules that matter here: a unit is loaded from the
// files present at the last daemon-reload (and stays loaded while active),
// start/restart/enable need it loaded, and starting one server while the
// other is active is recorded as a conflict — both bind port 8000 and the
// GPU, so a real host would fail there.
type fakeHost struct {
	cmds  []string
	files map[string][]byte
	rules []*rule

	legacyInstalled bool
	loaded          map[string]bool
	active          map[string]bool
	enabled         map[string]bool
	conflicts       []string
}

const (
	serverUnit = "vllm-server.service"
	legacyUnit = "vllm.service"
	timerUnit  = "vllm-drift-check.timer"
)

func (h *fakeHost) exists(p string) bool {
	_, ok := h.files[p]
	return ok
}

func (h *fakeHost) reload() {
	h.loaded = map[string]bool{
		serverUnit:                 h.exists(QuadletPath),
		legacyUnit:                 h.legacyInstalled,
		timerUnit:                  h.exists("/etc/systemd/system/vllm-drift-check.timer"),
		"vllm-drift-check.service": h.exists("/etc/systemd/system/vllm-drift-check.service"),
	}
}

func unitName(n string) string {
	if strings.Contains(n, ".") {
		return n
	}
	return n + ".service"
}

func (h *fakeHost) start(u string) {
	other := map[string]string{serverUnit: legacyUnit, legacyUnit: serverUnit}[u]
	if other != "" && h.active[other] {
		h.conflicts = append(h.conflicts, "started "+u+" while "+other+" was active")
	}
	h.active[u] = true
}

// systemctl runs one step of a systemctl command line.
func (h *fakeHost) systemctl(step string) (string, error) {
	f := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(step, "sudo "), "systemctl "), " 2>/dev/null"))
	verb, args := f[0], f[1:]
	now := len(args) > 0 && args[0] == "--now"
	if now {
		args = args[1:]
	}
	u := ""
	if len(args) > 0 {
		u = unitName(args[len(args)-1])
	}
	switch verb {
	case "daemon-reload":
		h.reload()
	case "is-active":
		if h.active[u] {
			return "active\n", nil
		}
		return "inactive\n", nil
	case "is-enabled":
		switch {
		case u == legacyUnit && !h.legacyInstalled:
			return "not-found\n", nil
		case h.enabled[u]:
			return "enabled\n", nil
		}
		return "disabled\n", nil
	case "show":
		if h.loaded[u] || h.active[u] {
			return "loaded\n", nil
		}
		return "not-found\n", nil
	case "start", "restart":
		if !h.loaded[u] {
			return "", fmt.Errorf("fake: Unit %s not found", u)
		}
		h.start(u)
	case "stop":
		if !h.loaded[u] && !h.active[u] {
			return "", fmt.Errorf("fake: Unit %s not loaded", u)
		}
		h.active[u] = false
	case "enable", "disable":
		if !h.loaded[u] {
			return "", fmt.Errorf("fake: Unit file %s does not exist", u)
		}
		h.enabled[u] = verb == "enable"
		if u == timerUnit {
			if verb == "enable" {
				h.files[timerWantsLink] = []byte("link")
			} else {
				delete(h.files, timerWantsLink)
			}
		}
		if now && verb == "enable" {
			h.start(u)
		}
		if now && verb == "disable" {
			h.active[u] = false
		}
	default:
		return "", fmt.Errorf("fake: unsupported systemctl step %q", step)
	}
	return "", nil
}

// steps splits a command line into its && steps, the unit a rule or an
// invariant reasons about.
func steps(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		for _, st := range strings.Split(c, "&&") {
			out = append(out, strings.TrimSpace(st))
		}
	}
	return out
}

func (h *fakeHost) match(cmd string) *rule {
	for _, r := range h.rules {
		if r.used || !strings.Contains(cmd, r.sub) {
			continue
		}
		if r.once {
			r.used = true
		}
		return r
	}
	return nil
}

var (
	reWrite   = regexp.MustCompile(`^echo '([^']*)' \| base64 -d \| sudo tee (\S+) >/dev/null$`)
	reProbe   = regexp.MustCompile(`^sudo sh -c 'test -e (\S+) \|\| echo absent'$`)
	reBackup  = regexp.MustCompile(`^sudo cp -p (\S+) (\S+)$`)
	reRestore = regexp.MustCompile(`^sudo cp (\S+) (\S+)$`)
	reMove    = regexp.MustCompile(`^sudo mv (\S+) (\S+)$`)
	reRemove  = regexp.MustCompile(`^sudo rm -f (\S+)$`)
	reChmod   = regexp.MustCompile(`^sudo chmod \S+ (\S+)$`)
	reHash    = regexp.MustCompile(`^sudo sh -c 'if test -e (\S+); then sha256sum \S+ 2>/dev/null; else echo missing; fi'$`)
	reCat     = regexp.MustCompile(`^sudo cat (\S+)( 2>/dev/null)?$`)
	reTest    = regexp.MustCompile(`^sudo test -e (\S+)$`)
	reTouch   = regexp.MustCompile(`^sudo touch (\S+)$`)
)

func (h *fakeHost) on(sub, out string) *fakeHost {
	h.rules = append(h.rules, &rule{sub: sub, out: out})
	return h
}

func (h *fakeHost) fail(sub string) *fakeHost {
	h.rules = append(h.rules, &rule{sub: sub, err: errFail})
	return h
}

func (h *fakeHost) failOnce(sub string) *fakeHost {
	h.rules = append(h.rules, &rule{sub: sub, err: errFail, once: true})
	return h
}

// prepend puts a rule ahead of the defaults, for overriding one.
func (h *fakeHost) prepend(r *rule) *fakeHost {
	h.rules = append([]*rule{r}, h.rules...)
	return h
}

func (h *fakeHost) Run(_ context.Context, cmd string) (string, error) {
	h.cmds = append(h.cmds, cmd)
	if strings.HasPrefix(cmd, "sudo systemctl ") || strings.HasPrefix(cmd, "systemctl ") {
		// Rules apply per step, so a chain that fails part-way leaves the
		// steps before the failure done, as the shell would.
		var out string
		for _, st := range steps([]string{cmd}) {
			var err error
			if r := h.match(st); r != nil {
				out, err = r.out, r.err
			} else {
				out, err = h.systemctl(st)
			}
			if err != nil {
				return out, err
			}
		}
		return out, nil
	}
	if r := h.match(cmd); r != nil {
		return r.out, r.err
	}

	if m := reWrite.FindStringSubmatch(cmd); m != nil {
		b, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			return "", err
		}
		h.files[m[2]] = b
		return "", nil
	}
	if m := reProbe.FindStringSubmatch(cmd); m != nil {
		if _, ok := h.files[m[1]]; !ok {
			return "absent\n", nil
		}
		return "", nil
	}
	if m := reHash.FindStringSubmatch(cmd); m != nil {
		b, ok := h.files[m[1]]
		if !ok {
			return "missing\n", nil
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:]) + "  " + m[1] + "\n", nil
	}
	for _, re := range []*regexp.Regexp{reBackup, reRestore, reMove} {
		if m := re.FindStringSubmatch(cmd); m != nil {
			b, ok := h.files[m[1]]
			if !ok {
				return "", fmt.Errorf("fake: %s: no such file", m[1])
			}
			h.files[m[2]] = b
			if re == reMove {
				delete(h.files, m[1])
			}
			return "", nil
		}
	}
	if m := reRemove.FindStringSubmatch(cmd); m != nil {
		delete(h.files, m[1])
		return "", nil
	}
	if m := reChmod.FindStringSubmatch(cmd); m != nil {
		if _, ok := h.files[m[1]]; !ok {
			return "", fmt.Errorf("fake: chmod %s: no such file", m[1])
		}
		return "", nil
	}
	if m := reCat.FindStringSubmatch(cmd); m != nil {
		b, ok := h.files[m[1]]
		if !ok {
			return "", fmt.Errorf("fake: cat %s: no such file", m[1])
		}
		return string(b), nil
	}
	if m := reTest.FindStringSubmatch(cmd); m != nil {
		if _, ok := h.files[m[1]]; !ok {
			return "", errFail
		}
		return "", nil
	}
	if m := reTouch.FindStringSubmatch(cmd); m != nil {
		h.files[m[1]] = nil
		return "", nil
	}
	return "", nil
}

const fortyGiB = "42949672960"

// newHost is a GPU host whose prerequisites are all in place and whose
// staging has room. legacy is the pre-Quadlet vllm.service: "active"
// (installed, enabled, running), "enabled" (installed and enabled but
// stopped) or "inactive" (never installed). Nothing about the vllm server
// itself is installed yet.
func newHost(legacy string) *fakeHost {
	h := &fakeHost{
		files:   map[string][]byte{"/etc/vllm/cdi-generated-for": []byte("550.90.07|1.18.2-1")},
		active:  map[string]bool{},
		enabled: map[string]bool{},
	}
	switch legacy {
	case "active":
		h.legacyInstalled, h.enabled[legacyUnit], h.active[legacyUnit] = true, true, true
	case "enabled":
		h.legacyInstalled, h.enabled[legacyUnit] = true, true
	case "inactive":
	default:
		panic("newHost: unknown legacy state " + legacy)
	}
	h.reload()
	return h.
		on("dpkg-query", dpkgReady).
		on("nvidia-smi", "550.90.07\n").
		on("/etc/default/prometheus-node-exporter", "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter\"\n").
		on("df --output=avail -B1 /var/lib/vllm", fortyGiB).
		on("df --output=avail -B1 /var/lib/containers", fortyGiB)
}

// install puts s on h as a successful apply would have left it.
func install(h *fakeHost, s Spec) *fakeHost {
	h.files[QuadletPath] = []byte(Quadlet(s))
	for _, f := range DriftFiles() {
		h.files[f.Path] = f.Content
	}
	h.files[AppliedPath] = NewApplied(s, "0ld", "someone@box", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)).JSON()
	h.files["/var/lib/vllm/hf/.staged-"+s.Model.Revision] = nil
	h.files[timerWantsLink] = []byte("link")
	h.reload()
	h.enabled[timerUnit], h.active[timerUnit] = true, true
	if !h.active[legacyUnit] {
		h.active[serverUnit] = true
	}
	return h
}

// fakeGate answers Wait from errs in order and nil once they run out. It
// logs "<gate>" into the host's command stream so ordering against the
// commands around it is asserted in one slice.
type fakeGate struct {
	h          *fakeHost
	errs       []error
	calls      int
	checkErr   error
	checkCalls int
}

func (g *fakeGate) Check(context.Context, Spec) error {
	g.h.cmds = append(g.h.cmds, "<check>")
	g.checkCalls++
	return g.checkErr
}

func (g *fakeGate) Wait(context.Context, Spec) error {
	g.h.cmds = append(g.h.cmds, "<gate>")
	i := g.calls
	g.calls++
	if i < len(g.errs) {
		return g.errs[i]
	}
	return nil
}

type fakeModels struct {
	entries []ModelEntry
	err     error
}

func (m fakeModels) Models(context.Context) ([]ModelEntry, error) { return m.entries, m.err }

// tickingClock advances one second per read, so every phase has a
// deterministic, non-zero duration.
func tickingClock() func() time.Time {
	t := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

const suffix = "20260928-120000"

func pinBackupSuffix(t *testing.T) {
	t.Helper()
	orig := hostconverge.Now
	hostconverge.Now = func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { hostconverge.Now = orig })
}

func target(h *fakeHost, g Gatekeeper) Target {
	return Target{Host: "192.168.1.50", Runner: h, Gate: g, Commit: "abc123", By: "dev@box", Now: tickingClock()}
}

// newerSpec differs from sampleSpec in image and args, the change a
// Renovate digest bump plus a flag tweak would make.
func newerSpec() Spec {
	s := sampleSpec()
	s.Image = "docker.io/vllm/vllm-openai:v0.25.0@sha256:" + strings.Repeat("d", 64)
	s.Args = append(append([]string(nil), s.Args...), "--enable-auto-tool-choice")
	return s
}

// ---- exact command text, mirrored from hostconverge and converge.go ----

func hashCmd(p string) string {
	return "sudo sh -c 'if test -e " + p + "; then sha256sum " + p + " 2>/dev/null; else echo missing; fi'"
}

func installCmds(f hostconverge.File, existed bool) []string {
	dir, base := f.Path[:strings.LastIndex(f.Path, "/")], f.Path[strings.LastIndex(f.Path, "/")+1:]
	tmp := dir + "/." + base + ".new." + suffix
	cmds := []string{
		"echo '" + base64.StdEncoding.EncodeToString(f.Content) + "' | base64 -d | sudo tee " + tmp + " >/dev/null",
		"sudo sh -c 'test -e " + f.Path + " || echo absent'",
	}
	if existed {
		cmds = append(cmds, "sudo cp -p "+f.Path+" "+f.Path+".backup."+suffix)
	}
	cmds = append(cmds, "sudo mv "+tmp+" "+f.Path)
	if f.Mode != 0 {
		cmds = append(cmds, fmt.Sprintf("sudo chmod %o %s", f.Mode.Perm(), f.Path))
	}
	return cmds
}

const (
	activateNewCmd       = "sudo systemctl daemon-reload && sudo systemctl restart vllm-server && sudo systemctl enable --now vllm-drift-check.timer"
	activateMigrateCmd   = "sudo systemctl daemon-reload && sudo systemctl disable --now vllm.service && sudo systemctl restart vllm-server && sudo systemctl enable --now vllm-drift-check.timer"
	stopServerCmd        = "sudo systemctl stop vllm-server"
	loadStateCmd         = "systemctl show -p LoadState --value vllm-server 2>/dev/null"
	stopTimerCmd         = "sudo systemctl stop vllm-drift-check.timer"
	unlinkTimerCmd       = "sudo rm -f " + timerWantsLink
	reloadCmd            = "sudo systemctl daemon-reload"
	startLegacyCmd       = "sudo systemctl enable --now vllm.service"
	enableLegacyCmd      = "sudo systemctl enable vllm.service"
	startOnlyLegacyCmd   = "sudo systemctl start vllm.service"
	rmMetricsCmd         = "sudo rm -f " + driftMetrics
	activatePreviousCmd  = "sudo systemctl daemon-reload && sudo systemctl restart vllm-server"
	startDriftCheckCmd   = "sudo systemctl start vllm-drift-check.service"
	wantProbeUnitCmd     = "sudo sh -c 'test -e " + QuadletPath + " || echo absent'"
	wantCatUnitCmd       = "sudo cat " + QuadletPath + " 2>/dev/null"
	wantRecordWriteQuery = "| base64 -d | sudo tee /etc/vllm/.applied.json.new."
)

func ensureHostNoOpCmds() []string {
	return []string{wantDpkgQueryCmd, wantMkdirCmd, wantNvidiaSmiCmd, wantCDIRecordReadCmd, wantNodeExporterGrepCmd}
}

func countContaining(cmds []string, sub string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func phaseNames(ps []Phase) []string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names
}

// ---- Apply ----

func TestApplyUnchangedDoesNotRestart(t *testing.T) {
	s := sampleSpec()
	h := install(newHost("inactive"), s)
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), s)

	require.Nil(t, res.Failure)
	assert.False(t, res.Changed)
	assert.Equal(t, ServingNew, res.Serving)
	assert.Empty(t, res.Reasons)
	assert.Zero(t, g.calls, "a no-op apply must not wait out a gate on a server it never touched")
	assert.Equal(t, 1, g.checkCalls, "a no-op apply still proves the server it leaves in place answers")
	assert.Equal(t, append(ensureHostNoOpCmds(),
		hashCmd(QuadletPath),
		hashCmd("/usr/local/libexec/vllm-drift-check"),
		hashCmd("/etc/systemd/system/vllm-drift-check.service"),
		hashCmd("/etc/systemd/system/vllm-drift-check.timer"),
		wantProbeAppliedCmd,
		wantCatAppliedCmd,
		wantIsActiveCmd,
		wantIsEnabledCmd,
		"<check>",
	), h.cmds)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
}

// A host that matches serving.yaml but whose server does not answer is
// down, not converged; talops reports it and leaves recovery to the
// runbook rather than restarting a unit it did not change.
func TestApplyUnchangedButUnhealthyReportsDown(t *testing.T) {
	s := sampleSpec()
	h := install(newHost("inactive"), s)
	g := &fakeGate{h: h, checkErr: errors.New("models check: connection refused")}

	res := Apply(context.Background(), target(h, g), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "connection refused")
	assert.False(t, res.Changed)
	assert.False(t, res.RolledBack)
	assert.Equal(t, ServingNone, res.Serving)
	assert.Equal(t, 1, g.checkCalls)
	assert.Zero(t, g.calls)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
	assert.Zero(t, countContaining(h.cmds, "base64 -d"))
	assert.Equal(t, "<check>", h.cmds[len(h.cmds)-1])
	help := strings.Join(res.Help, "\n")
	assert.Contains(t, help, "endpoint is down")
	assert.Contains(t, help, "scenarios/ai-sre-agent-runbook.md")
	assert.NotContains(t, help, "rollback", "nothing was rolled back")
	assert.Equal(t, []string{"host", "gate"}, phaseNames(res.Phases))
}

// cancelOnCheck cancels ctx once Check runs, the way an operator's Ctrl-C
// lands while the no-change apply's single health check is in flight.
type cancelOnCheck struct {
	*fakeGate
	cancel context.CancelFunc
}

func (g cancelOnCheck) Check(ctx context.Context, s Spec) error {
	err := g.fakeGate.Check(ctx, s)
	g.cancel()
	return err
}

// An interrupt during the no-change apply's single Check is not proof the
// server is down — unlike TestApplyUnchangedButUnhealthyReportsDown, apply
// never got to find out, so it must not send the operator to the runbook
// for a server it never actually examined.
func TestApplyInterruptedDuringTheNoChangeCheckIsNotReportedAsDown(t *testing.T) {
	s := sampleSpec()
	h := install(newHost("inactive"), s)
	fg := &fakeGate{h: h, checkErr: errors.New("models check: context canceled")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := cancelOnCheck{fakeGate: fg, cancel: cancel}

	res := Apply(ctx, target(h, g), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeStage, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "cancelled")
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.False(t, res.Changed)
	assert.False(t, res.RolledBack)
	assert.Equal(t, 1, fg.checkCalls)
	help := strings.Join(res.Help, "\n")
	assert.NotContains(t, help, "endpoint is down", "nothing was found to be down; the check was interrupted")
	assert.Contains(t, help, "vllm apply --confirm", "re-running apply checks again")
}

// Each thing apply compares is, on its own, enough to make it act: a
// record that disagrees with a unit that matches is still a host whose
// provenance is wrong, and a legacy unit still serving is not converged.
func TestApplyActsOnEachSingleDifference(t *testing.T) {
	cases := map[string]func(h *fakeHost, s Spec){
		"unit":        func(h *fakeHost, s Spec) { h.files[QuadletPath] = []byte("edited by hand\n") },
		"drift files": func(h *fakeHost, s Spec) { h.files["/etc/systemd/system/vllm-drift-check.timer"] = []byte("old\n") },
		"no record":   func(h *fakeHost, s Spec) { delete(h.files, AppliedPath) },
		"record args": func(h *fakeHost, s Spec) {
			a := NewApplied(s, "0ld", "x", time.Time{})
			a.ArgsHash = strings.Repeat("0", 64)
			h.files[AppliedPath] = a.JSON()
		},
		"record digest": func(h *fakeHost, s Spec) {
			a := NewApplied(s, "0ld", "x", time.Time{})
			a.ImageDigest = "sha256:" + strings.Repeat("0", 64)
			h.files[AppliedPath] = a.JSON()
		},
		"record revision": func(h *fakeHost, s Spec) {
			a := NewApplied(s, "0ld", "x", time.Time{})
			a.ModelRevision = strings.Repeat("0", 40)
			h.files[AppliedPath] = a.JSON()
		},
		"legacy active": func(h *fakeHost, s Spec) {
			h.legacyInstalled, h.enabled[legacyUnit], h.active[legacyUnit], h.active[serverUnit] = true, true, true, false
			h.reload()
		},
		"legacy enabled but stopped": func(h *fakeHost, s Spec) {
			h.legacyInstalled, h.enabled[legacyUnit] = true, true
			h.reload()
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pinBackupSuffix(t)
			s := sampleSpec()
			h := install(newHost("inactive"), s)
			mutate(h, s)
			g := &fakeGate{h: h}

			res := Apply(context.Background(), target(h, g), s)

			require.Nil(t, res.Failure, "%+v", res.Failure)
			assert.True(t, res.Changed)
			assert.Len(t, res.Reasons, 1, "%v", res.Reasons)
			assert.Equal(t, 1, g.calls)
			assert.Equal(t, Quadlet(s), string(h.files[QuadletPath]))
		})
	}
}

func TestApplyStagingFailureTouchesNothing(t *testing.T) {
	old := sampleSpec()
	h := install(newHost("inactive"), old)
	before := map[string]string{}
	for p, b := range h.files {
		before[p] = string(b)
	}
	h.fail("sudo podman pull")
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeStage, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "pull image")
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.True(t, res.Changed, "apply had something to do; it just did not get to do it")
	assert.False(t, res.RolledBack)
	assert.Zero(t, g.calls)
	assert.Zero(t, countContaining(h.cmds, "sudo mv"))
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
	assert.Zero(t, countContaining(h.cmds, "base64 -d"))
	after := map[string]string{}
	for p, b := range h.files {
		after[p] = string(b)
	}
	assert.Equal(t, before, after)
	assert.Equal(t, []string{"host", "stage"}, phaseNames(res.Phases))
	assert.Contains(t, res.Help, "nothing changed on the running server")
}

// cancelOn cancels the apply's context once a command containing sub has
// run, the way an operator's Ctrl-C lands while that step is in flight. The
// fake host ignores ctx, so the step itself still completes: what is under
// test is that apply does not go on to the swap.
type cancelOn struct {
	*fakeHost
	sub    string
	cancel context.CancelFunc
}

func (c cancelOn) Run(ctx context.Context, cmd string) (string, error) {
	out, err := c.fakeHost.Run(ctx, cmd)
	if strings.Contains(cmd, c.sub) {
		c.cancel()
	}
	return out, err
}

func TestApplyInterruptedBeforeTheSwapTouchesNothing(t *testing.T) {
	for name, sub := range map[string]string{
		"during host prerequisites": "nvidia-smi",
		"during staging":            "sudo podman pull",
		"during the GPU check":      "--device nvidia.com/gpu=all",
	} {
		t.Run(name, func(t *testing.T) {
			h := install(newHost("inactive"), sampleSpec())
			before := map[string]string{}
			for p, b := range h.files {
				before[p] = string(b)
			}
			g := &fakeGate{h: h}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tg := target(h, g)
			tg.Runner = cancelOn{fakeHost: h, sub: sub, cancel: cancel}

			res := Apply(ctx, tg, newerSpec())

			require.NotNil(t, res.Failure)
			assert.Equal(t, CodeStage, res.Failure.Code)
			assert.Contains(t, res.Failure.Msg, "cancelled before the swap")
			assert.Equal(t, ServingPrevious, res.Serving)
			assert.False(t, res.RolledBack)
			assert.Zero(t, g.calls)
			assert.Zero(t, countContaining(h.cmds, "sudo systemctl"), "nothing may be restarted")
			assert.Zero(t, countContaining(h.cmds, "sudo mv"))
			assert.Zero(t, countContaining(h.cmds, "base64 -d"))
			last := -1
			for i, c := range h.cmds {
				if strings.Contains(c, sub) {
					last = i
				}
			}
			require.GreaterOrEqual(t, last, 0)
			assert.Zero(t, countContaining(h.cmds[last:], wantIsActiveCmd), "legacy state is only re-read on the way to the swap")
			after := map[string]string{}
			for p, b := range h.files {
				after[p] = string(b)
			}
			assert.Equal(t, before, after)
			assert.Contains(t, res.Help, "nothing changed on the running server")
		})
	}
}

// The swap is the one step that interrupts consumers, so a host whose CDI
// spec cannot give the new image the GPU must be caught before it: on the
// first apply that found this, the swap stopped the legacy server, the new
// one failed on "unresolvable CDI devices", and only the rollback brought
// the old one back.
func TestApplyUnresolvableCDITouchesNothing(t *testing.T) {
	for _, legacy := range []string{"active", "inactive"} {
		t.Run("legacy "+legacy, func(t *testing.T) {
			pinBackupSuffix(t)
			h := newHost(legacy)
			if legacy == "inactive" {
				install(h, sampleSpec())
			}
			// Staged up front, so the only writes left to catch are the swap's.
			h.files["/var/lib/vllm/hf/.staged-"+newerSpec().Model.Revision] = nil
			before := map[string]string{}
			for p, b := range h.files {
				before[p] = string(b)
			}
			wasActive := map[string]bool{}
			for u, a := range h.active {
				wasActive[u] = a
			}
			h.prepend(&rule{sub: "--device nvidia.com/gpu=all", out: "Error: setting up CDI devices: unresolvable CDI devices nvidia.com/gpu=all\n", err: errFail})
			g := &fakeGate{h: h}

			res := Apply(context.Background(), target(h, g), newerSpec())

			require.NotNil(t, res.Failure)
			assert.Equal(t, CodeCDIUnresolvable, res.Failure.Code)
			assert.Contains(t, res.Failure.Msg, "CDI")
			assert.Equal(t, ServingPrevious, res.Serving)
			assert.True(t, res.Changed)
			assert.False(t, res.RolledBack)
			assert.Zero(t, g.calls)
			assert.Zero(t, countContaining(h.cmds, "sudo systemctl"), "nothing may be restarted")
			assert.Zero(t, countContaining(h.cmds, "base64 -d"))
			assert.Equal(t, wantGPUCheckCmd(newerSpec().Image), h.cmds[len(h.cmds)-1], "nothing runs after a failed check")
			after := map[string]string{}
			for p, b := range h.files {
				after[p] = string(b)
			}
			assert.Equal(t, before, after)
			assert.Equal(t, wasActive, h.active)
			assert.Equal(t, []string{"host", "stage", "gpu-check"}, phaseNames(res.Phases))
			help := strings.Join(res.Help, "\n")
			assert.Contains(t, help, "nothing changed on the running server")
			assert.Contains(t, help, "docs/vllm-serving.md#troubleshooting")
			assert.NotContains(t, help, "endpoint is down")
		})
	}
}

// The GPU check's run can fail without podman saying anything about CDI — SSH
// dropping, broken container storage, a runtime error. That still stops
// before the swap, but under its own code, so the operator is not sent to
// the CDI spec for a problem that is not one.
func TestApplyGPUCheckFailureThatIsNotCDIHasItsOwnCode(t *testing.T) {
	pinBackupSuffix(t)
	h := install(newHost("inactive"), sampleSpec())
	h.files["/var/lib/vllm/hf/.staged-"+newerSpec().Model.Revision] = nil
	before := map[string]string{}
	for p, b := range h.files {
		before[p] = string(b)
	}
	wasActive := map[string]bool{}
	for u, a := range h.active {
		wasActive[u] = a
	}
	h.prepend(&rule{sub: "--device nvidia.com/gpu=all", out: "Error: creating container storage: layer not known\n", err: errFail})
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGPUCheck, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "could not verify")
	assert.Contains(t, res.Failure.Msg, "fake failure", "the underlying error is shown")
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.True(t, res.Changed)
	assert.False(t, res.RolledBack)
	assert.Zero(t, g.calls)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"), "nothing may be restarted")
	assert.Zero(t, countContaining(h.cmds, "base64 -d"))
	assert.Equal(t, wantGPUCheckCmd(newerSpec().Image), h.cmds[len(h.cmds)-1], "nothing runs after a failed check")
	after := map[string]string{}
	for p, b := range h.files {
		after[p] = string(b)
	}
	assert.Equal(t, before, after)
	assert.Equal(t, wasActive, h.active)
	assert.Equal(t, []string{"host", "stage", "gpu-check"}, phaseNames(res.Phases))
	help := strings.Join(res.Help, "\n")
	assert.Contains(t, help, "nothing changed on the running server")
	assert.Contains(t, help, "GPU could not be verified")
	assert.NotContains(t, help, "unresolvable CDI devices")
	assert.NotContains(t, help, "endpoint is down")
}

// A package query that could not be run stops the apply as a host
// prerequisite failure, before anything is installed or restarted.
func TestApplyPackageQueryFailureInstallsNothing(t *testing.T) {
	h := install(newHost("inactive"), sampleSpec())
	h.prepend(&rule{sub: "dpkg-query", err: errFail})
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeHostPrereq, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "read installed packages")
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.False(t, res.RolledBack)
	assert.Zero(t, g.calls)
	assert.Zero(t, countContaining(h.cmds, "apt-get"), "nothing may be installed")
	assert.Zero(t, countContaining(h.cmds, "apt-mark"))
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"), "nothing may be restarted")
}

// A GPU check that fails because the operator interrupted it says nothing
// about the CDI spec, so it is reported as the interrupt it was.
func TestApplyInterruptedGPUCheckIsACancelNotACDIFailure(t *testing.T) {
	h := install(newHost("inactive"), sampleSpec())
	h.prepend(&rule{sub: "--device nvidia.com/gpu=all", err: context.Canceled})
	g := &fakeGate{h: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tg := target(h, g)
	tg.Runner = cancelOn{fakeHost: h, sub: "--device nvidia.com/gpu=all", cancel: cancel}

	res := Apply(ctx, tg, newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeStage, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "cancelled before the swap")
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
}

func TestApplyDownloadFailureTouchesNothing(t *testing.T) {
	h := newHost("active") // legacy serving, model never staged
	h.fail("snapshot_download")
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeStage, res.Failure.Code)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"), "the legacy server must keep running")
	assert.NotContains(t, h.files, QuadletPath)
}

func TestApplyHappyPathOrder(t *testing.T) {
	pinBackupSuffix(t)
	s := sampleSpec()
	h := newHost("inactive")
	g := &fakeGate{h: h}
	tg := target(h, g)

	res := Apply(context.Background(), tg, s)

	require.Nil(t, res.Failure, "%+v", res.Failure)
	assert.True(t, res.Changed)
	assert.False(t, res.RolledBack)
	assert.Equal(t, ServingNew, res.Serving)

	unit := quadletFile(s)
	drift := DriftFiles()
	// The record's appliedAt is the clock read the Apply issued for it;
	// recover it from what landed on disk rather than recomputing the tick.
	rec, err := ReadApplied(context.Background(), h)
	require.NoError(t, err)
	h.cmds = h.cmds[:len(h.cmds)-2] // drop this test's own ReadApplied probe + cat
	recFile := hostconverge.File{Path: AppliedPath, Content: rec.JSON(), Mode: 0o644}

	want := ensureHostNoOpCmds()
	want = append(want,
		hashCmd(QuadletPath), // pending: unit missing short-circuits
		hashCmd(drift[0].Path),
		wantProbeAppliedCmd,
		wantIsActiveCmd,
		wantIsEnabledCmd,
		wantMarkerProbeCmd(s.Model.Revision), // stage
		wantDfVllmCmd,
		wantDfContainersCmd,
		wantPullCmd(s.Image),
		wantDownloadCmd(s.Image, s.Model.Repo, s.Model.Revision),
		wantTouchMarkerCmd(s.Model.Revision),
		wantGPUCheckCmd(s.Image), // before anything the swap touches
		wantIsActiveCmd,          // legacy re-read after staging
		wantIsEnabledCmd,
	)
	want = append(want, installCmds(unit, false)...)
	for _, f := range drift {
		want = append(want, installCmds(f, false)...)
	}
	want = append(want, wantProbeUnitCmd, hashCmd(QuadletPath), activateNewCmd, "<gate>")
	want = append(want, installCmds(recFile, false)...)
	want = append(want, startDriftCheckCmd)
	assert.Equal(t, want, h.cmds)

	assert.Equal(t, "abc123", rec.Commit)
	assert.Equal(t, "dev@box", rec.AppliedBy)
	assert.Equal(t, ArgsHash(s), rec.ArgsHash)
	assert.Equal(t, []string{"host", "stage", "gpu-check", "converge", "gate", "record"}, phaseNames(res.Phases))
	for _, p := range res.Phases {
		assert.Positive(t, p.Took, p.Name)
	}
}

func TestApplyGateFailureRollsBack(t *testing.T) {
	pinBackupSuffix(t)
	old, s := sampleSpec(), newerSpec()
	h := install(newHost("inactive"), old)
	oldRecord := string(h.files[AppliedPath])
	g := &fakeGate{h: h, errs: []error{errors.New("models check: no entry")}}

	res := Apply(context.Background(), target(h, g), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "models check: no entry")
	assert.Contains(t, res.Failure.Msg, "(rolled back)")
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.Equal(t, 2, g.calls, "the restored server must be gated too")
	assert.Equal(t, Quadlet(old), string(h.files[QuadletPath]))
	assert.Equal(t, oldRecord, string(h.files[AppliedPath]), "the record names what serves; it must not claim the rolled-back spec")
	assert.Zero(t, countContaining(h.cmds, wantRecordWriteQuery))
	assert.Zero(t, countContaining(h.cmds, startDriftCheckCmd))

	gate := indexOf(h.cmds, "<gate>")
	require.Positive(t, gate)
	assert.Equal(t, activateNewCmd, h.cmds[gate-1])
	assert.Equal(t, []string{
		"<gate>",
		"sudo cp /etc/systemd/system/vllm-drift-check.timer.backup." + suffix + " /etc/systemd/system/vllm-drift-check.timer",
		"sudo cp /etc/systemd/system/vllm-drift-check.service.backup." + suffix + " /etc/systemd/system/vllm-drift-check.service",
		"sudo cp /usr/local/libexec/vllm-drift-check.backup." + suffix + " /usr/local/libexec/vllm-drift-check",
		"sudo cp " + QuadletPath + ".backup." + suffix + " " + QuadletPath,
		wantProbeUnitCmd,
		hashCmd(QuadletPath),
		activatePreviousCmd,
		"<gate>",
	}, h.cmds[gate:])
	assert.Equal(t, []string{"host", "stage", "gpu-check", "converge", "gate", "gate-rollback"}, phaseNames(res.Phases))
	assert.Contains(t, res.Help, "the previous server is serving again and passed the health gate")
	assert.Contains(t, strings.Join(res.Help, "\n"), "talops vllm status")
}

func TestApplyReportsRolledBackButUnhealthy(t *testing.T) {
	pinBackupSuffix(t)
	h := install(newHost("inactive"), sampleSpec())
	g := &fakeGate{h: h, errs: []error{errors.New("tool call check: no tool_calls"), errors.New("models check: connection refused")}}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "unhealthy too")
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingNone, res.Serving)
	help := strings.Join(res.Help, "\n")
	assert.Contains(t, help, "endpoint is down")
	assert.Contains(t, help, "scenarios/ai-sre-agent-runbook.md")
	assert.NotContains(t, help, "previous server is serving")
}

// identityGate passes Wait only for a spec naming the identity the server
// on the host actually has, the way the real gate reads /v1/models, and
// records which identity every Wait was asked about.
type identityGate struct {
	h          *fakeHost
	servedName string
	modelRepo  string
	asked      []string
}

func (g *identityGate) Check(context.Context, Spec) error { return nil }

func (g *identityGate) Wait(_ context.Context, s Spec) error {
	g.h.cmds = append(g.h.cmds, "<gate>")
	g.asked = append(g.asked, s.ServedName+" "+s.Model.Repo)
	if s.ServedName != g.servedName || s.Model.Repo != g.modelRepo {
		return fmt.Errorf("models check: no entry with id %q", s.ServedName)
	}
	return nil
}

func TestApplyRollbackGatesTheRestoredServersIdentity(t *testing.T) {
	pinBackupSuffix(t)
	old := sampleSpec()
	s := newerSpec()
	s.ServedName = "qwen/qwen3-next"
	s.Model = Model{Repo: "Qwen/Qwen3-Next-80B-A3B-Instruct-AWQ", Revision: strings.Repeat("c", 40)}
	h := install(newHost("inactive"), old)
	// The new model never comes up, so the old one is all that ever answers.
	g := &identityGate{h: h, servedName: old.ServedName, modelRepo: old.Model.Repo}

	res := Apply(context.Background(), target(h, g), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingPrevious, res.Serving, "the restored server serves the previous model and must be gated as that model")
	assert.Equal(t, []string{
		s.ServedName + " " + s.Model.Repo,
		old.ServedName + " " + old.Model.Repo,
	}, g.asked)
	assert.Equal(t, Quadlet(old), string(h.files[QuadletPath]))
	assert.Contains(t, res.Help, "the previous server is serving again and passed the health gate")
}

func TestApplyFirstRunRollbackRestoresLegacyUnit(t *testing.T) {
	pinBackupSuffix(t)
	s := sampleSpec()
	h := newHost("active")
	h.files[driftMetrics] = []byte("vllm_serving_drift{reason=\"none\"} 1\n") // written while the rejected server ran
	g := &fakeGate{h: h, errs: []error{errors.New("completion check: status 500")}}

	res := Apply(context.Background(), target(h, g), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.NotContains(t, h.files, QuadletPath, "rollback of a first apply removes the unit it added")

	migrate := indexOf(h.cmds, activateMigrateCmd)
	require.NotEqual(t, -1, migrate, "forward activation must disable the legacy unit")
	removed := indexOf(h.cmds, "sudo rm -f "+QuadletPath)
	require.Greater(t, removed, migrate)
	assert.Equal(t, []string{
		wantProbeUnitCmd, // the branch is chosen from the unit on disk
		stopServerCmd,
		stopTimerCmd,
		unlinkTimerCmd,
		rmMetricsCmd,
		reloadCmd,
		startLegacyCmd,
		"<gate>", // the legacy server is gated before rollback reports success
	}, h.cmds[removed+1:])

	assert.True(t, h.active[legacyUnit], "legacy server running again")
	assert.True(t, h.enabled[legacyUnit])
	assert.False(t, h.active[serverUnit], "the rejected server must not keep the port and GPU")
	assert.Empty(t, h.conflicts)
	assert.NotContains(t, h.files, timerWantsLink)
	assert.NotContains(t, h.files, driftMetrics, "a stale metric would keep reporting the rejected spec")
}

func TestApplyFirstRunRollbackLeavesAnEnabledButStoppedLegacyUnitAsItWas(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("enabled")
	g := &fakeGate{h: h, errs: []error{errFail}}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingNone, res.Serving, "nothing served before, so nothing serves now")
	assert.True(t, h.enabled[legacyUnit], "enablement restored")
	assert.False(t, h.active[legacyUnit], "and not started: it was not running before")
	assert.False(t, h.active[serverUnit])
	assert.Equal(t, 1, g.calls, "no rollback gate against a server that is not meant to run")
	assert.Contains(t, res.Help[0], "nothing was serving before")
}

// Nothing served before this apply, so the rollback has nothing to bring
// back: it stops and removes the new server and must say the endpoint is
// down rather than "previous serving".
func TestApplyFirstRunWithoutLegacyRollsBackToNoServer(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("inactive")
	g := &fakeGate{h: h, errs: []error{errors.New("tool call check: no tool_calls")}}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeGate, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, "no previous server to restore")
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingNone, res.Serving)
	assert.Equal(t, 1, g.calls, "there is no previous server to gate")
	assert.NotContains(t, h.files, QuadletPath)
	assert.NotContains(t, h.files, timerWantsLink)
	assert.False(t, h.active[serverUnit])
	assert.False(t, h.active[timerUnit])
	assert.Zero(t, countContaining(h.cmds, startLegacyCmd))
	help := strings.Join(res.Help, "\n")
	assert.Contains(t, help, "first apply")
	assert.Contains(t, help, "scenarios/ai-sre-agent-runbook.md")
}

// A legacy unit that is enabled but stopped would start at boot beside the
// new server, so the migration disables it too.
func TestApplyDisablesAnEnabledButStoppedLegacyUnit(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("enabled")

	res := Apply(context.Background(), target(h, &fakeGate{h: h}), sampleSpec())

	require.Nil(t, res.Failure)
	assert.Contains(t, h.cmds, activateMigrateCmd)
	assert.False(t, h.enabled[legacyUnit], "legacy must not start at the next boot")
	assert.False(t, h.active[legacyUnit])
	assert.True(t, h.active[serverUnit])
	assert.Empty(t, h.conflicts)
}

// A failure starting the legacy unit again is the worst case of a first
// apply: the new server failed and the old one did not come back.
func TestApplyFirstRunRollbackReportsLegacyThatWillNotStart(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("active").fail("enable --now vllm.service")
	g := &fakeGate{h: h, errs: []error{errFail}}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, ServingNone, res.Serving)
	assert.Contains(t, res.Failure.Msg, "could not activate")
	assert.Equal(t, 1, g.calls)
	assert.Contains(t, strings.Join(res.Help, "\n"), "scenarios/ai-sre-agent-runbook.md")
}

func TestApplyActivateFailureRollsBackAsConvergeFailure(t *testing.T) {
	pinBackupSuffix(t)
	old := sampleSpec()
	h := install(newHost("inactive"), old).failOnce("sudo systemctl restart vllm-server")
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeConverge, res.Failure.Code)
	assert.True(t, res.RolledBack)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.Equal(t, 1, g.calls, "only the restored server is gated")
	assert.Equal(t, Quadlet(old), string(h.files[QuadletPath]))
	assert.Equal(t, []string{"host", "stage", "gpu-check", "converge", "gate-rollback"}, phaseNames(res.Phases))
}

// The only gate that runs here is the rollback's; its failure is the
// previous server being down, not the new one failing its gate.
func TestApplyActivateFailureWithUnhealthyRollbackIsNotAGateFailure(t *testing.T) {
	pinBackupSuffix(t)
	h := install(newHost("inactive"), sampleSpec()).failOnce("sudo systemctl restart vllm-server")
	g := &fakeGate{h: h, errs: []error{errFail}}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeConverge, res.Failure.Code)
	assert.Equal(t, ServingNone, res.Serving)
	assert.Equal(t, []string{"host", "stage", "gpu-check", "converge", "gate-rollback"}, phaseNames(res.Phases))
}

// An install that fails part-way never reaches Activate: the running server
// was never restarted, so it is still the one serving.
func TestApplyInstallFailureLeavesPreviousServing(t *testing.T) {
	pinBackupSuffix(t)
	old := sampleSpec()
	h := install(newHost("inactive"), old).fail("sudo mv /etc/systemd/system/.vllm-drift-check.service.new")
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeConverge, res.Failure.Code)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
	assert.Equal(t, Quadlet(old), string(h.files[QuadletPath]))
	assert.Contains(t, res.Help, "nothing changed on the running server")
}

func TestApplyRecordFailureStillReportsTheNewServer(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("inactive").fail(wantRecordWriteQuery)
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeRecord, res.Failure.Code)
	assert.Contains(t, res.Failure.Msg, AppliedPath)
	assert.Equal(t, ServingNew, res.Serving)
	assert.False(t, res.RolledBack)
	assert.Zero(t, countContaining(h.cmds, startDriftCheckCmd))
	assert.Contains(t, strings.Join(res.Help, "\n"), "passed the health gate")
}

func TestApplyDriftCheckStartFailureIsANote(t *testing.T) {
	pinBackupSuffix(t)
	h := newHost("inactive").fail(startDriftCheckCmd)
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	assert.Nil(t, res.Failure)
	assert.Equal(t, ServingNew, res.Serving)
	require.Len(t, res.Notes, 1)
	assert.Contains(t, res.Notes[0], "drift check did not run once")
}

func TestApplyHostPrereqFailure(t *testing.T) {
	h := newHost("active").prepend(&rule{sub: "nvidia-smi", err: errFail})
	g := &fakeGate{h: h}

	res := Apply(context.Background(), target(h, g), sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeHostPrereq, res.Failure.Code)
	assert.Equal(t, ServingPrevious, res.Serving)
	assert.False(t, res.Changed)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
	assert.Equal(t, []string{"host"}, phaseNames(res.Phases))
}

func TestApplyReportsHostWarningsAsNotes(t *testing.T) {
	s := sampleSpec()
	h := install(newHost("inactive"), s).prepend(&rule{sub: "/etc/default/prometheus-node-exporter", out: "ARGS=\"--collector.textfile.directory=/srv/textfiles\"\n"})

	res := Apply(context.Background(), target(h, &fakeGate{h: h}), s)

	require.Nil(t, res.Failure)
	assert.False(t, res.Changed, "a warning is not a change")
	require.Len(t, res.Notes, 1)
	assert.True(t, strings.HasPrefix(res.Notes[0], "host: warning: "), res.Notes[0])
}

func TestApplyReadFailures(t *testing.T) {
	cases := map[string]struct {
		sub  string
		code string
	}{
		"unit hash":      {sub: hashCmd(QuadletPath), code: CodeRead},
		"record probe":   {sub: wantProbeAppliedCmd, code: CodeRead},
		"legacy unknown": {sub: wantIsActiveCmd, code: CodeLegacyUnknown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := install(newHost("inactive"), sampleSpec()).prepend(&rule{sub: tc.sub, err: errFail})
			g := &fakeGate{h: h}

			res := Apply(context.Background(), target(h, g), newerSpec())

			require.NotNil(t, res.Failure)
			assert.Equal(t, tc.code, res.Failure.Code)
			assert.Equal(t, ServingPrevious, res.Serving)
			assert.Zero(t, countContaining(h.cmds, "podman pull"))
			assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
		})
	}
}

// Staging takes minutes; the legacy state is read again after it, and a
// failure then must stop the swap rather than guess which unit to replace.
func TestApplyLegacyUnknownAfterStagingStopsBeforeTheSwap(t *testing.T) {
	h := install(newHost("inactive"), sampleSpec()).
		prepend(&rule{sub: wantIsActiveCmd, out: "bogus\n"}).
		prepend(&rule{sub: wantIsActiveCmd, out: "inactive\n", once: true})

	res := Apply(context.Background(), target(h, &fakeGate{h: h}), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeLegacyUnknown, res.Failure.Code)
	assert.Equal(t, 1, countContaining(h.cmds, "podman pull"))
	assert.Zero(t, countContaining(h.cmds, "base64 -d"))
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
}

func TestApplyWithoutGateRefusesBeforeTouchingTheHost(t *testing.T) {
	h := newHost("inactive")
	tg := target(h, nil)
	tg.Gate = nil

	res := Apply(context.Background(), tg, sampleSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeConfigInvalid, res.Failure.Code)
	assert.Empty(t, h.cmds)
}

// ---- activate: the three branches the rollback depends on ----

var legacyRunning = LegacyState{Active: true, Enabled: true}

func TestActivateBranches(t *testing.T) {
	s := sampleSpec()

	withNewUnit := func(h *fakeHost) *fakeHost {
		h.files[QuadletPath] = []byte(Quadlet(s))
		for _, f := range DriftFiles() {
			h.files[f.Path] = f.Content
		}
		return h
	}

	t.Run("a: new unit on disk, migrating from legacy", func(t *testing.T) {
		h := withNewUnit(newHost("active"))
		require.NoError(t, activate(s, legacyRunning)(context.Background(), h))
		assert.Equal(t, []string{wantProbeUnitCmd, hashCmd(QuadletPath), activateMigrateCmd}, h.cmds)
	})

	t.Run("a: new unit on disk, no legacy", func(t *testing.T) {
		h := withNewUnit(newHost("inactive"))
		require.NoError(t, activate(s, LegacyState{})(context.Background(), h))
		assert.Equal(t, []string{wantProbeUnitCmd, hashCmd(QuadletPath), activateNewCmd}, h.cmds)
	})

	t.Run("b: previous unit restored by rollback", func(t *testing.T) {
		for _, legacy := range []LegacyState{legacyRunning, {}} {
			h := install(newHost("inactive"), newerSpec())
			require.NoError(t, activate(s, legacy)(context.Background(), h))
			assert.Equal(t, []string{wantProbeUnitCmd, hashCmd(QuadletPath), activatePreviousCmd}, h.cmds, "legacy=%+v", legacy)
		}
	})

	t.Run("c: unit removed by rollback, legacy was present", func(t *testing.T) {
		h := newHost("inactive")
		h.legacyInstalled = true
		h.files[QuadletPath] = []byte(Quadlet(s))
		h.reload()
		h.active[serverUnit] = true
		delete(h.files, QuadletPath)

		require.NoError(t, activate(s, legacyRunning)(context.Background(), h))
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, stopTimerCmd, unlinkTimerCmd, rmMetricsCmd, reloadCmd, startLegacyCmd}, h.cmds)
		assert.False(t, h.active[serverUnit])
		assert.True(t, h.active[legacyUnit])
		assert.Empty(t, h.conflicts)
	})

	// A forward activation that failed before its daemon-reload never
	// loaded vllm-server; stopping it fails, and that alone must not keep
	// the legacy server from coming back.
	t.Run("c: new server never loaded", func(t *testing.T) {
		h := newHost("active")
		h.active[legacyUnit] = false

		require.NoError(t, activate(s, legacyRunning)(context.Background(), h))
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, loadStateCmd, stopTimerCmd, unlinkTimerCmd, rmMetricsCmd, reloadCmd, startLegacyCmd}, h.cmds)
		assert.True(t, h.active[legacyUnit])
	})

	t.Run("c: new server loaded but will not stop", func(t *testing.T) {
		h := newHost("inactive").prepend(&rule{sub: stopServerCmd, err: errFail})
		h.legacyInstalled = true
		h.active[serverUnit] = true

		err := activate(s, legacyRunning)(context.Background(), h)
		require.ErrorIs(t, err, errNewServerWillNotStop)
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, loadStateCmd}, h.cmds, "legacy must not start beside a server still holding the port")
		assert.False(t, h.active[legacyUnit])
	})

	t.Run("d: new server loaded but will not stop", func(t *testing.T) {
		h := newHost("inactive").prepend(&rule{sub: stopServerCmd, err: errFail})
		h.active[serverUnit] = true

		err := activate(s, LegacyState{})(context.Background(), h)
		require.ErrorIs(t, err, errNewServerWillNotStop)
		assert.NotErrorIs(t, err, errNoPreviousServer, "the stop failure is the cause to act on")
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, loadStateCmd}, h.cmds)
	})

	// The legacy unit gets back exactly the pair it had: enabled but
	// stopped stays stopped, and since nothing served before, the rollback
	// has no server to report.
	t.Run("c: legacy was enabled but stopped", func(t *testing.T) {
		h := newHost("enabled")
		h.enabled[legacyUnit] = false // the forward swap disabled it

		err := activate(s, LegacyState{Enabled: true})(context.Background(), h)
		require.ErrorIs(t, err, errNoPreviousServer)
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, loadStateCmd, stopTimerCmd, unlinkTimerCmd, rmMetricsCmd, reloadCmd, enableLegacyCmd}, h.cmds)
		assert.True(t, h.enabled[legacyUnit])
		assert.False(t, h.active[legacyUnit])
	})

	t.Run("c: legacy was running but not enabled", func(t *testing.T) {
		h := newHost("enabled")
		h.enabled[legacyUnit] = false

		require.NoError(t, activate(s, LegacyState{Active: true})(context.Background(), h))
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, loadStateCmd, stopTimerCmd, unlinkTimerCmd, rmMetricsCmd, reloadCmd, startOnlyLegacyCmd}, h.cmds)
		assert.False(t, h.enabled[legacyUnit])
		assert.True(t, h.active[legacyUnit])
	})

	t.Run("d: unit removed by rollback, no legacy", func(t *testing.T) {
		h := newHost("inactive")
		h.files[QuadletPath] = []byte(Quadlet(s))
		h.reload()
		h.active[serverUnit] = true
		delete(h.files, QuadletPath)

		err := activate(s, LegacyState{})(context.Background(), h)
		require.ErrorIs(t, err, errNoPreviousServer)
		assert.Equal(t, []string{wantProbeUnitCmd, stopServerCmd, stopTimerCmd, unlinkTimerCmd, rmMetricsCmd, reloadCmd}, h.cmds)
		assert.False(t, h.active[serverUnit])
	})

	t.Run("probe failure is an error, never a guess", func(t *testing.T) {
		for _, legacy := range []LegacyState{legacyRunning, {}} {
			h := newHost("active").fail(wantProbeUnitCmd)
			err := activate(s, legacy)(context.Background(), h)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "check installed unit")
			assert.Equal(t, []string{wantProbeUnitCmd}, h.cmds, "legacy=%+v", legacy)
		}
	})

	t.Run("hash failure is an error, never a guess", func(t *testing.T) {
		for _, legacy := range []LegacyState{legacyRunning, {}} {
			h := newHost("active").fail("sha256sum")
			h.files[QuadletPath] = []byte(Quadlet(s))
			err := activate(s, legacy)(context.Background(), h)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "read installed unit")
			assert.Equal(t, []string{wantProbeUnitCmd, hashCmd(QuadletPath)}, h.cmds, "legacy=%+v", legacy)
		}
	})

	t.Run("systemctl failure is returned", func(t *testing.T) {
		h := newHost("inactive").fail("daemon-reload")
		h.files[QuadletPath] = []byte(Quadlet(s))
		err := activate(s, LegacyState{})(context.Background(), h)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "activate")
	})
}

// ---- Status ----

// liveHost is a converged host whose container runs s.
func liveHost(s Spec) *fakeHost {
	h := install(newHost("inactive"), s)
	return h.
		on("'{{.State.Running}}'", "true\n").
		on("'{{.Image}}'", strings.Repeat("c", 64)+"\n").
		on("RepoDigests", "docker.io/vllm/vllm-openai@"+s.ImageDigest()+"\n").
		on("Config.Cmd", strings.Join(ExecArgs(s), "\x00")+"\n")
}

func servingModels(s Spec) fakeModels {
	return fakeModels{entries: []ModelEntry{{ID: s.ServedName, Root: s.Model.Repo}}}
}

func statusTarget(h *fakeHost, m ModelsReader) Target {
	return Target{Host: "192.168.1.50", Runner: h, Models: m}
}

func fieldByName(t *testing.T, fs []Field, name string) Field {
	t.Helper()
	for _, f := range fs {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q in %+v", name, fs)
	return Field{}
}

func TestStatusConvergedHasNoDrift(t *testing.T) {
	s := sampleSpec()
	h := liveHost(s)

	res := Status(context.Background(), statusTarget(h, servingModels(s)), s)

	require.Nil(t, res.Failure)
	assert.False(t, res.Drift, "%+v", res.Fields)
	assert.False(t, res.Legacy)
	assert.Empty(t, res.Help)
	assert.Equal(t, []Field{
		{Name: "imageDigest", Git: s.ImageDigest(), Applied: s.ImageDigest(), Live: s.ImageDigest()},
		{Name: "modelRevision", Git: s.Model.Revision, Applied: s.Model.Revision, Live: "n/a"},
		{Name: "servedName", Git: s.ServedName, Applied: s.ServedName, Live: s.ServedName},
		{Name: "argsHash", Git: ArgsHash(s), Applied: ArgsHash(s), Live: ArgsHash(s)},
	}, res.Fields)
	assert.Zero(t, countContaining(h.cmds, "sudo systemctl"))
	assert.Zero(t, countContaining(h.cmds, "base64 -d"))
}

func TestStatusFlagsEachMismatch(t *testing.T) {
	s := sampleSpec()
	other := newerSpec()

	cases := []struct {
		name   string
		field  string
		setup  func(h *fakeHost) ModelsReader
		column func(f Field) (got, want string)
	}{
		{
			name:  "imageDigest",
			field: "imageDigest",
			setup: func(h *fakeHost) ModelsReader {
				h.prepend(&rule{sub: "RepoDigests", out: "docker.io/vllm/vllm-openai@" + other.ImageDigest() + "\n"})
				return servingModels(s)
			},
			column: func(f Field) (string, string) {
				return f.Live, "docker.io/vllm/vllm-openai@" + other.ImageDigest()
			},
		},
		{
			name:  "modelRevision",
			field: "modelRevision",
			setup: func(h *fakeHost) ModelsReader {
				a := NewApplied(s, "0ld", "x", time.Time{})
				a.ModelRevision = strings.Repeat("e", 40)
				h.files[AppliedPath] = a.JSON()
				return servingModels(s)
			},
			column: func(f Field) (string, string) { return f.Applied, strings.Repeat("e", 40) },
		},
		{
			name:  "servedName",
			field: "servedName",
			setup: func(h *fakeHost) ModelsReader {
				return fakeModels{entries: []ModelEntry{{ID: "qwen/something-else", Root: s.Model.Repo}}}
			},
			column: func(f Field) (string, string) { return f.Live, "qwen/something-else" },
		},
		{
			name:  "argsHash",
			field: "argsHash",
			setup: func(h *fakeHost) ModelsReader {
				h.prepend(&rule{sub: "Config.Cmd", out: strings.Join(ExecArgs(other), "\x00") + "\n"})
				return servingModels(s)
			},
			column: func(f Field) (string, string) { return f.Live, ArgsHash(other) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := liveHost(s)
			m := tc.setup(h)

			res := Status(context.Background(), statusTarget(h, m), s)

			require.Nil(t, res.Failure)
			assert.True(t, res.Drift)
			assert.False(t, res.Legacy)
			got, want := tc.column(fieldByName(t, res.Fields, tc.field))
			assert.Equal(t, want, got)
			for _, f := range res.Fields {
				if f.Name == tc.field {
					continue
				}
				assert.Equal(t, f.Git, f.Applied, "only %s should differ: %+v", tc.field, f)
				if f.Live != "n/a" {
					assert.Equal(t, f.Git, f.Live, "only %s should differ: %+v", tc.field, f)
				}
			}
			assert.Contains(t, strings.Join(res.Help, "\n"), "talops vllm apply --confirm")
		})
	}

	t.Run("legacy", func(t *testing.T) {
		h := liveHost(s).prepend(&rule{sub: wantIsActiveCmd, out: "active\n"})

		res := Status(context.Background(), statusTarget(h, servingModels(s)), s)

		require.Nil(t, res.Failure)
		assert.True(t, res.Legacy)
		assert.True(t, res.Drift, "every column agrees, but the legacy unit is still active")
		for _, f := range res.Fields {
			assert.Equal(t, f.Git, f.Applied)
		}
	})
}

// A git change nobody has applied yet: applied and live agree with each
// other and both differ from git. The live digest is shown as the recorded
// one, not as an opaque per-arch reference, so the operator sees the
// server is exactly what was last applied.
func TestStatusShowsUnappliedGitChange(t *testing.T) {
	old, s := sampleSpec(), newerSpec()
	h := liveHost(old)

	res := Status(context.Background(), statusTarget(h, servingModels(old)), s)

	assert.True(t, res.Drift)
	img := fieldByName(t, res.Fields, "imageDigest")
	assert.Equal(t, Field{Name: "imageDigest", Git: s.ImageDigest(), Applied: old.ImageDigest(), Live: old.ImageDigest()}, img)
	args := fieldByName(t, res.Fields, "argsHash")
	assert.Equal(t, args.Applied, args.Live)
	assert.NotEqual(t, args.Git, args.Live)
}

func TestStatusNotRunningAndUnreachable(t *testing.T) {
	s := sampleSpec()
	h := install(newHost("inactive"), s) // no podman rules: inspect answers "" → not running

	res := Status(context.Background(), statusTarget(h, fakeModels{err: errFail}), s)

	require.Nil(t, res.Failure)
	assert.True(t, res.Drift)
	assert.Equal(t, "(not running)", fieldByName(t, res.Fields, "imageDigest").Live)
	assert.Equal(t, "(not running)", fieldByName(t, res.Fields, "argsHash").Live)
	assert.Equal(t, "unreachable", fieldByName(t, res.Fields, "servedName").Live)
}

func TestStatusNeverAppliedShowsNone(t *testing.T) {
	s := sampleSpec()
	h := liveHost(s)
	delete(h.files, AppliedPath)

	res := Status(context.Background(), statusTarget(h, servingModels(s)), s)

	require.Nil(t, res.Failure)
	assert.True(t, res.Drift)
	for _, f := range res.Fields {
		assert.Equal(t, "(none)", f.Applied, f.Name)
	}
}

func TestStatusKeepsGoingPastReadFailures(t *testing.T) {
	s := sampleSpec()
	h := liveHost(s).
		prepend(&rule{sub: wantProbeAppliedCmd, err: errFail}).
		prepend(&rule{sub: wantIsActiveCmd, out: "bogus\n"})

	res := Status(context.Background(), statusTarget(h, servingModels(s)), s)

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeRead, res.Failure.Code)
	require.Len(t, res.Notes, 1)
	assert.True(t, strings.HasPrefix(res.Notes[0], CodeLegacyUnknown+": "), res.Notes[0])
	assert.True(t, res.Drift, "an unreadable record is not a converged one")
	for _, f := range res.Fields {
		assert.Equal(t, "unknown", f.Applied, f.Name)
	}
	assert.Equal(t, s.ImageDigest(), fieldByName(t, res.Fields, "imageDigest").Live, "the live column is still read")
}

// ---- Plan ----

func TestPlanListsConsumersOnlyWhenChanged(t *testing.T) {
	s := sampleSpec()

	t.Run("converged", func(t *testing.T) {
		h := install(newHost("inactive"), s)
		res := Plan(context.Background(), statusTarget(h, nil), s)

		require.Nil(t, res.Failure)
		assert.False(t, res.Changed)
		assert.Empty(t, res.Diff)
		assert.Empty(t, res.Reasons)
		assert.Nil(t, res.Consumers)
		assert.Empty(t, res.Help)
	})

	t.Run("unit changed", func(t *testing.T) {
		h := install(newHost("inactive"), s)
		res := Plan(context.Background(), statusTarget(h, nil), newerSpec())

		require.Nil(t, res.Failure)
		assert.True(t, res.Changed)
		assert.Equal(t, Consumers, res.Consumers)
		assert.Contains(t, res.Diff, "--- deployed "+QuadletPath+"\n+++ rendered\n")
		assert.Contains(t, res.Diff, "\n-Image="+s.Image+"\n")
		assert.Contains(t, res.Diff, "\n+Image="+newerSpec().Image+"\n")
		assert.Contains(t, res.Diff, "--enable-auto-tool-choice")
		assert.Contains(t, res.Reasons, "unit "+QuadletPath+" differs from serving.yaml")
	})

	t.Run("only the legacy unit differs", func(t *testing.T) {
		h := install(newHost("active"), s)
		res := Plan(context.Background(), statusTarget(h, nil), s)

		require.Nil(t, res.Failure)
		assert.True(t, res.Changed, "apply would act, so plan must say so")
		assert.Empty(t, res.Diff)
		assert.Equal(t, Consumers, res.Consumers)
		require.Len(t, res.Reasons, 1)
		assert.Contains(t, res.Reasons[0], "legacy vllm.service")
	})

	t.Run("first apply", func(t *testing.T) {
		h := newHost("active")
		res := Plan(context.Background(), statusTarget(h, nil), s)

		require.Nil(t, res.Failure)
		assert.True(t, res.Changed)
		assert.Equal(t, Consumers, res.Consumers)
		assert.Contains(t, res.Diff, "\n+Image="+s.Image+"\n")
		assert.NotContains(t, res.Diff, "\n-")
		assert.Equal(t, []string{wantProbeUnitCmd}, h.cmds[:1], "an absent unit is probed, never read as empty")
		assert.NotContains(t, h.cmds, wantCatUnitCmd)
	})
}

func TestPlanMutatesNothing(t *testing.T) {
	h := install(newHost("active"), sampleSpec())
	_ = Plan(context.Background(), statusTarget(h, nil), newerSpec())
	for _, c := range h.cmds {
		for _, forbidden := range []string{"base64 -d", "sudo mv", "sudo cp", "sudo rm", "sudo systemctl", "podman pull", "apt-get"} {
			assert.NotContains(t, c, forbidden)
		}
	}
}

func TestPlanReadFailure(t *testing.T) {
	h := install(newHost("inactive"), sampleSpec()).prepend(&rule{sub: wantCatUnitCmd, err: errFail})
	res := Plan(context.Background(), statusTarget(h, nil), newerSpec())

	require.NotNil(t, res.Failure)
	assert.Equal(t, CodeRead, res.Failure.Code)
	assert.False(t, res.Changed)
	assert.Nil(t, res.Consumers)
}

// The generated unit of the Quadlet is vllm-server.service; vllm.service is
// only ever the legacy unit. A bare "restart vllm" would resolve to the
// legacy unit, and disabling a generated unit is refused by systemd, so
// neither may appear in any path apply can take.
func TestApplyNeverAddressesTheWrongUnit(t *testing.T) {
	assert.Equal(t, "/etc/containers/systemd/vllm-server.container", QuadletPath)

	bareRestart := regexp.MustCompile(`restart vllm(\.service)?(\s|$|;|&)`)
	scenarios := map[string]func() (*fakeHost, *fakeGate, Spec){
		"first apply, healthy": func() (*fakeHost, *fakeGate, Spec) {
			h := newHost("inactive")
			return h, &fakeGate{h: h}, sampleSpec()
		},
		"migration, healthy": func() (*fakeHost, *fakeGate, Spec) {
			h := newHost("active")
			return h, &fakeGate{h: h}, sampleSpec()
		},
		"migration, rolled back": func() (*fakeHost, *fakeGate, Spec) {
			h := newHost("active")
			return h, &fakeGate{h: h, errs: []error{errFail}}, sampleSpec()
		},
		"update, rolled back": func() (*fakeHost, *fakeGate, Spec) {
			h := install(newHost("inactive"), sampleSpec())
			return h, &fakeGate{h: h, errs: []error{errFail}}, newerSpec()
		},
		"update, activate fails": func() (*fakeHost, *fakeGate, Spec) {
			h := install(newHost("inactive"), sampleSpec()).failOnce("restart vllm-server")
			return h, &fakeGate{h: h}, newerSpec()
		},
		"no-op, unhealthy": func() (*fakeHost, *fakeGate, Spec) {
			h := install(newHost("inactive"), sampleSpec())
			return h, &fakeGate{h: h, checkErr: errFail}, sampleSpec()
		},
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) {
			pinBackupSuffix(t)
			h, g, s := setup()
			_ = Apply(context.Background(), target(h, g), s)
			for _, c := range h.cmds {
				for _, step := range strings.Split(c, "&&") {
					assert.False(t, bareRestart.MatchString(step), "bare restart of vllm: %s", c)
					if strings.Contains(step, "disable") {
						assert.NotContains(t, step, "vllm-server", "disabling the generated unit: %s", c)
					}
				}
			}
		})
	}
}

// Invariants every rollback must keep, checked on the command log and on the
// emulated systemd state after it:
//
//   - the new server is stopped before the legacy unit is started, since both
//     bind port 8000 and the GPU;
//   - nothing restarts vllm-server once the rollback removed its Quadlet;
//   - a rollback that removed the Quadlet also retired the drift timer, which
//     would otherwise fire a check whose script is gone, and its metric file,
//     which nothing would refresh again;
//   - the legacy unit ends with exactly the enabled and running state it had;
//   - the two servers are never both running.
func TestRollbackInvariants(t *testing.T) {
	type scenario struct {
		host    func() *fakeHost
		gate    []error
		spec    Spec
		serving string
	}
	always := []error{errFail, errFail}
	scenarios := map[string]scenario{
		"migration, gate fails": {
			host: func() *fakeHost { return newHost("active") }, gate: []error{errFail}, spec: sampleSpec(), serving: ServingPrevious,
		},
		"migration from enabled-but-stopped legacy, gate fails": {
			host: func() *fakeHost { return newHost("enabled") }, gate: []error{errFail}, spec: sampleSpec(), serving: ServingNone,
		},
		"migration from enabled-but-stopped legacy, new server will not start": {
			host:    func() *fakeHost { return newHost("enabled").failOnce("restart vllm-server") },
			spec:    sampleSpec(),
			serving: ServingNone,
		},
		"migration from enabled-but-stopped legacy, reload fails before the new server loads": {
			host:    func() *fakeHost { return newHost("enabled").failOnce("daemon-reload") },
			spec:    sampleSpec(),
			serving: ServingNone,
		},
		"migration from enabled-but-stopped legacy, timer will not enable": {
			host:    func() *fakeHost { return newHost("enabled").failOnce("enable --now vllm-drift-check.timer") },
			spec:    sampleSpec(),
			serving: ServingNone,
		},
		"migration, timer will not enable": {
			host:    func() *fakeHost { return newHost("active").failOnce("enable --now vllm-drift-check.timer") },
			spec:    sampleSpec(),
			serving: ServingPrevious,
		},
		"first apply without legacy, timer will not enable": {
			host:    func() *fakeHost { return newHost("inactive").failOnce("enable --now vllm-drift-check.timer") },
			spec:    sampleSpec(),
			serving: ServingNone,
		},
		"update, timer will not enable": {
			host: func() *fakeHost {
				return install(newHost("inactive"), sampleSpec()).failOnce("enable --now vllm-drift-check.timer")
			},
			spec:    newerSpec(),
			serving: ServingPrevious,
		},
		"migration, legacy unhealthy too": {
			host: func() *fakeHost { return newHost("active") }, gate: always, spec: sampleSpec(), serving: ServingNone,
		},
		"migration, new server will not start": {
			host:    func() *fakeHost { return newHost("active").failOnce("restart vllm-server") },
			spec:    sampleSpec(),
			serving: ServingPrevious,
		},
		"migration, reload fails before the new server loads": {
			host:    func() *fakeHost { return newHost("active").failOnce("daemon-reload") },
			spec:    sampleSpec(),
			serving: ServingPrevious,
		},
		"first apply without legacy, gate fails": {
			host: func() *fakeHost { return newHost("inactive") }, gate: []error{errFail}, spec: sampleSpec(), serving: ServingNone,
		},
		"first apply without legacy, new server will not start": {
			host:    func() *fakeHost { return newHost("inactive").failOnce("restart vllm-server") },
			spec:    sampleSpec(),
			serving: ServingNone,
		},
		"update, gate fails": {
			host: func() *fakeHost { return install(newHost("inactive"), sampleSpec()) }, gate: []error{errFail}, spec: newerSpec(), serving: ServingPrevious,
		},
		"update, new server will not start": {
			host:    func() *fakeHost { return install(newHost("inactive"), sampleSpec()).failOnce("restart vllm-server") },
			spec:    newerSpec(),
			serving: ServingPrevious,
		},
		"update, previous unhealthy too": {
			host: func() *fakeHost { return install(newHost("inactive"), sampleSpec()) }, gate: always, spec: newerSpec(), serving: ServingNone,
		},
	}

	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			pinBackupSuffix(t)
			h := sc.host()
			h.files[driftMetrics] = []byte("metric\n")
			legacyBefore := LegacyState{Active: h.active[legacyUnit], Enabled: h.enabled[legacyUnit]}
			res := Apply(context.Background(), target(h, &fakeGate{h: h, errs: sc.gate}), sc.spec)

			require.NotNil(t, res.Failure)
			require.True(t, res.RolledBack)
			assert.Equal(t, sc.serving, res.Serving)
			assert.Empty(t, h.conflicts, "both servers running at once")

			st := steps(h.cmds)
			lastStart, removed := -1, -1
			for i, step := range st {
				switch step {
				case "sudo systemctl restart vllm-server":
					lastStart = i
					assert.Equal(t, -1, removed, "restart of vllm-server after its Quadlet was removed")
				case "sudo rm -f " + QuadletPath:
					removed = i
				case startLegacyCmd, startOnlyLegacyCmd:
					stopped := false
					for _, prev := range st[lastStart+1 : i] {
						stopped = stopped || prev == stopServerCmd
					}
					assert.True(t, stopped, "vllm.service started without stopping vllm-server first")
				}
			}

			if !h.exists(QuadletPath) {
				assert.Contains(t, st, stopTimerCmd)
				assert.False(t, h.active[timerUnit], "drift timer still running")
				assert.NotContains(t, h.files, timerWantsLink, "drift timer still enabled")
				assert.False(t, h.active[serverUnit], "the removed server still running")
				assert.NotContains(t, h.files, driftMetrics, "stale drift metric for the removed server")
			} else {
				assert.Contains(t, h.files, driftMetrics, "the previous server's drift metric is still true")
			}
			assert.Equal(t, legacyBefore, LegacyState{Active: h.active[legacyUnit], Enabled: h.enabled[legacyUnit]},
				"the legacy unit must end exactly as the apply found it")
			if h.active[legacyUnit] {
				assert.False(t, h.active[serverUnit])
			}
		})
	}
}

// Each way a rollback can leave no server needs a different recovery, so
// each gets its own first help line.
func TestApplyDownHelpNamesTheCause(t *testing.T) {
	cases := map[string]struct {
		host func() *fakeHost
		gate []error
		want string
	}{
		"restore failed": {
			host: func() *fakeHost {
				return install(newHost("inactive"), sampleSpec()).fail("sudo cp " + QuadletPath + ".backup.")
			},
			gate: []error{errFail},
			want: "could not restore the previous files",
		},
		"previous will not start": {
			host: func() *fakeHost {
				return install(newHost("inactive"), sampleSpec()).fail("restart vllm-server")
			},
			want: "could not start the previous server",
		},
		"previous unhealthy": {
			host: func() *fakeHost { return install(newHost("inactive"), sampleSpec()) },
			gate: []error{errFail, errFail},
			want: "fails the health gate too",
		},
		"new server will not stop, legacy waiting": {
			host: func() *fakeHost {
				return newHost("active").prepend(&rule{sub: stopServerCmd, err: errFail})
			},
			gate: []error{errFail},
			want: "the new server would not stop; stop vllm-server by hand before starting anything else, see scenarios/ai-sre-agent-runbook.md",
		},
		"new server will not stop, no legacy": {
			host: func() *fakeHost {
				return newHost("inactive").prepend(&rule{sub: stopServerCmd, err: errFail})
			},
			gate: []error{errFail},
			want: "the new server would not stop",
		},
		"no previous server": {
			host: func() *fakeHost { return newHost("inactive") },
			gate: []error{errFail},
			want: "nothing was serving before",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pinBackupSuffix(t)
			h := tc.host()
			res := Apply(context.Background(), target(h, &fakeGate{h: h, errs: tc.gate}), newerSpec())

			require.NotNil(t, res.Failure)
			assert.Equal(t, ServingNone, res.Serving)
			assert.Contains(t, res.Help[0], "endpoint is down")
			assert.Contains(t, res.Help[0], tc.want)
			assert.Contains(t, strings.Join(res.Help, "\n"), "scenarios/ai-sre-agent-runbook.md")
		})
	}
}

func TestStatusFlagsAnEnabledButStoppedLegacyUnit(t *testing.T) {
	s := sampleSpec()
	h := liveHost(s)
	h.legacyInstalled, h.enabled[legacyUnit] = true, true

	res := Status(context.Background(), statusTarget(h, servingModels(s)), s)

	require.Nil(t, res.Failure)
	assert.True(t, res.Legacy, "an enabled legacy unit starts at boot beside the new server")
	assert.True(t, res.Drift)
}
