package haproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// DiffLimit caps the diff a plan prints by default. A generated config is a few
// kilobytes, so this shows most drift in full while keeping a whole-file rewrite
// from filling the caller's context.
const DiffLimit = 1500

// Unknown is the third answer a check can give. A layer that could not be read
// is not the same as a layer that read clean, and collapsing the two is how a
// dark load balancer gets reported as healthy.
const Unknown = "unknown"

// Failure is a machine-readable reason a command could not answer, paired with
// prose the caller can act on.
type Failure struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (f *Failure) Error() string { return f.Msg }

// VMInfo describes the virtual machine behind the push target.
//
// Source is the load-bearing field: "terraform" means the address belongs to a
// VM this repo can rebuild, "unmanaged" means it does not — which is the honest
// answer for a load balancer built by hand before the Terraform config existed.
type VMInfo struct {
	Name   string `json:"name,omitempty"`
	VMID   int    `json:"vmid,omitempty"`
	Node   string `json:"node,omitempty"`
	State  string `json:"state"`
	Source string `json:"source"`
}

// StatusResult is one-shot health across every layer between the caller and the
// backends: VM, SSH, service, config, and per-server health.
type StatusResult struct {
	Host        string       `json:"host"`
	VM          VMInfo       `json:"vm"`
	SSH         string       `json:"ssh"`
	Service     string       `json:"service"`
	ConfigDrift string       `json:"configDrift"`
	Backends    []ServerStat `json:"-"`
	Fields      []string     `json:"-"`
	Notes       []string     `json:"notes,omitempty"`
	Failure     *Failure     `json:"error,omitempty"`
	Help        []string     `json:"-"`
}

// PlanResult is the difference between the config the generator renders from
// current cluster state and the one installed on the host.
type PlanResult struct {
	Host         string   `json:"host"`
	Drift        bool     `json:"drift"`
	Backends     int      `json:"backends"`
	RenderedHash string   `json:"renderedHash"`
	DeployedHash string   `json:"deployedHash,omitempty"`
	Diff         string   `json:"-"`
	DiffBytes    int      `json:"diffBytes"`
	Truncated    bool     `json:"truncated"`
	Notes        []string `json:"notes,omitempty"`
	Failure      *Failure `json:"error,omitempty"`
	Help         []string `json:"-"`
}

// ApplyResult is the outcome of a config push.
type ApplyResult struct {
	Host         string   `json:"host"`
	Changed      bool     `json:"changed"`
	Drift        bool     `json:"drift"`
	Backends     int      `json:"backends"`
	RenderedHash string   `json:"renderedHash"`
	DryRun       bool     `json:"dryRun"`
	Notes        []string `json:"notes,omitempty"`
	Failure      *Failure `json:"error,omitempty"`
	Help         []string `json:"-"`
}

// ReportStatus renders a StatusResult as TOON.
func ReportStatus(res StatusResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  vm: %s\n", inlineVM(res.VM))
	fmt.Fprintf(&b, "  ssh: %s\n", orDash(res.SSH))
	fmt.Fprintf(&b, "  service: %s\n", orDash(res.Service))
	fmt.Fprintf(&b, "  configDrift: %s\n", orDash(res.ConfigDrift))

	fields := res.Fields
	if len(fields) == 0 {
		fields = DefaultStatFields
	}

	if len(res.Backends) == 0 {
		// Say the zero out loud. An empty section reads as "the check did not
		// run" and invites a second call with different flags.
		b.WriteString("backends: 0 server rows reported by HAProxy\n")
	} else {
		fmt.Fprintf(&b, "backends[%d]{%s}:\n", len(res.Backends), strings.Join(fields, ","))
		for _, stat := range res.Backends {
			cells := make([]string, 0, len(fields))
			for _, f := range fields {
				cells = append(cells, toonCell(stat.Field(f)))
			}
			fmt.Fprintf(&b, "  %s\n", strings.Join(cells, ","))
		}
		fmt.Fprintf(&b, "backendsUp: %d/%d\n", UpCount(res.Backends), len(res.Backends))
	}

	writeNotes(&b, res.Notes)
	writeFailure(&b, res.Failure)
	writeHelp(&b, res.Help)
	return b.String()
}

// ReportPlan renders a PlanResult as TOON. A converged config is stated
// explicitly rather than shown as an absent diff section.
func ReportPlan(res PlanResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  drift: %t\n", res.Drift)
	fmt.Fprintf(&b, "  backends: %d\n", res.Backends)
	fmt.Fprintf(&b, "  renderedHash: %s\n", shortHash(res.RenderedHash))
	if res.DeployedHash != "" {
		fmt.Fprintf(&b, "  deployedHash: %s\n", shortHash(res.DeployedHash))
	}

	switch {
	case res.Failure != nil:
	case !res.Drift:
		b.WriteString("diff: 0 changes — the deployed config already matches current cluster state\n")
	default:
		b.WriteString("diff: |\n")
		for _, line := range strings.Split(strings.TrimRight(res.Diff, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
		if res.Truncated {
			fmt.Fprintf(&b, "  ... (truncated, %d chars total — use --full)\n", res.DiffBytes)
		}
	}

	writeNotes(&b, res.Notes)
	writeFailure(&b, res.Failure)
	writeHelp(&b, res.Help)
	return b.String()
}

// ReportApply renders an ApplyResult as TOON.
func ReportApply(res ApplyResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  changed: %t\n", res.Changed)
	fmt.Fprintf(&b, "  drift: %t\n", res.Drift)
	fmt.Fprintf(&b, "  backends: %d\n", res.Backends)
	fmt.Fprintf(&b, "  renderedHash: %s\n", shortHash(res.RenderedHash))
	if res.DryRun {
		b.WriteString("  dryRun: true\n")
	}
	if res.Failure == nil && !res.Changed {
		b.WriteString("result: 0 changes pushed — the deployed config was already current\n")
	}

	writeNotes(&b, res.Notes)
	writeFailure(&b, res.Failure)
	writeHelp(&b, res.Help)
	return b.String()
}

// ReportError renders a failure on its own, for the case where no result could
// be assembled at all. The help lines carry the command that fixes it so the
// caller self-corrects in one turn rather than following up with --help.
func ReportError(f *Failure, help []string) string {
	var b strings.Builder
	writeFailure(&b, f)
	writeHelp(&b, help)
	return b.String()
}

// EmitJSON writes one newline-delimited JSON object describing a state
// transition. Every command emits at least a terminal object, so a caller
// parsing --json output never has to infer the outcome from an empty stream.
func EmitJSON(w io.Writer, event string, payload any) error {
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

// BackendsJSON projects the selected columns of each server row for --json.
func BackendsJSON(stats []ServerStat, fields []string) []map[string]string {
	if len(fields) == 0 {
		fields = DefaultStatFields
	}
	rows := make([]map[string]string, 0, len(stats))
	for _, s := range stats {
		row := make(map[string]string, len(fields))
		for _, f := range fields {
			row[f] = s.Field(f)
		}
		rows = append(rows, row)
	}
	return rows
}

func inlineVM(vm VMInfo) string {
	parts := make([]string, 0, 5)
	if vm.Name != "" {
		parts = append(parts, "name: "+vm.Name)
	}
	if vm.VMID > 0 {
		parts = append(parts, fmt.Sprintf("vmid: %d", vm.VMID))
	}
	if vm.Node != "" {
		parts = append(parts, "node: "+vm.Node)
	}
	parts = append(parts, "state: "+orDash(vm.State), "source: "+orDash(vm.Source))
	return "{" + strings.Join(parts, ", ") + "}"
}

func writeNotes(b *strings.Builder, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(b, "notes[%d]:\n", len(notes))
	for _, n := range notes {
		fmt.Fprintf(b, "  %s\n", n)
	}
}

func writeFailure(b *strings.Builder, f *Failure) {
	if f == nil {
		return
	}
	fmt.Fprintf(b, "error: {code: %s, msg: %q}\n", f.Code, f.Msg)
}

func writeHelp(b *strings.Builder, help []string) {
	if len(help) == 0 {
		return
	}
	fmt.Fprintf(b, "help[%d]:\n", len(help))
	for _, h := range help {
		fmt.Fprintf(b, "  %s\n", h)
	}
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

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return orDash(h)
}
