package haproxy

import (
	"fmt"
	"strings"
)

// Holder values that are not an instance name.
const (
	HolderNone  = "none"
	HolderSplit = "split"
)

// InstanceStatus is one group member's health across every layer.
type InstanceStatus struct {
	Instance
	VM         VMInfo `json:"vm"`
	SSH        string `json:"ssh"`
	Service    string `json:"service"`
	Keepalived string `json:"keepalived"`
	// HoldsAddress is "true", "false", or Unknown when the instance could not
	// be asked.
	HoldsAddress  string       `json:"holdsVirtualAddress"`
	ConfigDrift   string       `json:"configDrift"`
	BackendsUp    int          `json:"backendsUp"`
	BackendsTotal int          `json:"backendsTotal"`
	Backends      []ServerStat `json:"-"`
}

// GroupStatusResult is the health of a VRRP group: each instance, and which of
// them holds the virtual address.
type GroupStatusResult struct {
	VirtualAddress string           `json:"virtualAddress"`
	Holder         string           `json:"holder"`
	Instances      []InstanceStatus `json:"instances"`
	// BackendsFrom names the instance whose backend table is printed. Each
	// instance health-checks on its own, so the tables can disagree; the
	// holder's is the one clients are being served from.
	BackendsFrom string       `json:"backendsFrom,omitempty"`
	Backends     []ServerStat `json:"-"`
	Fields       []string     `json:"-"`
	Notes        []string     `json:"notes,omitempty"`
	Failure      *Failure     `json:"error,omitempty"`
	Help         []string     `json:"-"`
}

// Assess derives the holder and the group-level verdict from the instances.
//
// Every state short of "all members reachable, all able to take over, exactly
// one holding the address" is a failure, because each of them is a group that
// would not survive the next host loss — and that is the only thing the group
// is for. The checks run worst-first so the most urgent one is the one named.
func (r *GroupStatusResult) Assess() {
	var holders, unread, unreachable, idle []string
	for _, inst := range r.Instances {
		switch inst.HoldsAddress {
		case "true":
			holders = append(holders, inst.Label())
		case "false":
		default:
			unread = append(unread, inst.Label())
		}
		if inst.SSH != "ok" {
			unreachable = append(unreachable, inst.Label())
		} else if inst.Keepalived != "active" {
			idle = append(idle, inst.Label())
		}
	}

	switch {
	case len(holders) > 1:
		r.Holder = HolderSplit
	case len(holders) == 1:
		r.Holder = holders[0]
	case len(unread) > 0:
		r.Holder = Unknown
	default:
		r.Holder = HolderNone
	}

	r.BackendsFrom, r.Backends = "", nil
	for _, inst := range r.Instances {
		if inst.Label() == r.Holder {
			r.BackendsFrom, r.Backends = inst.Label(), inst.Backends
		}
	}
	for _, inst := range r.Instances {
		if r.BackendsFrom == "" && len(inst.Backends) > 0 {
			r.BackendsFrom, r.Backends = inst.Label(), inst.Backends
		}
	}

	switch {
	case r.Failure != nil:
	case len(holders) > 1:
		r.Failure = &Failure{Code: "virtual_address_split", Msg: fmt.Sprintf(
			"%s is configured on %s at once — clients reach whichever answered ARP last",
			r.VirtualAddress, strings.Join(holders, " and "))}
	case len(holders) == 0 && len(unread) == 0:
		r.Failure = &Failure{Code: "virtual_address_unheld", Msg: fmt.Sprintf(
			"no instance holds %s — the endpoint is dark", r.VirtualAddress)}
	case len(unreachable) > 0:
		r.Failure = &Failure{Code: "instance_unreachable", Msg: fmt.Sprintf(
			"%s could not be reached over SSH — the group has no redundancy until it returns",
			strings.Join(unreachable, ", "))}
	case len(unread) > 0:
		r.Failure = &Failure{Code: "instance_unread", Msg: fmt.Sprintf(
			"could not read the interface addresses on %s, so a second holder cannot be ruled out",
			strings.Join(unread, ", "))}
	case len(idle) > 0:
		r.Failure = &Failure{Code: "keepalived_inactive", Msg: fmt.Sprintf(
			"keepalived is not active on %s — it cannot take the virtual address over",
			strings.Join(idle, ", "))}
	}
}

// ReportGroupStatus renders a GroupStatusResult as TOON.
func ReportGroupStatus(res GroupStatusResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  virtualAddress: %s\n", orDash(res.VirtualAddress))
	fmt.Fprintf(&b, "  holder: %s\n", orDash(res.Holder))

	fmt.Fprintf(&b, "instances[%d]{name,host,vm,ssh,service,keepalived,holdsAddress,configDrift,backendsUp}:\n",
		len(res.Instances))
	for _, inst := range res.Instances {
		up := "-"
		if inst.BackendsTotal > 0 {
			up = fmt.Sprintf("%d/%d", inst.BackendsUp, inst.BackendsTotal)
		}
		cells := []string{
			inst.Name, inst.Host, vmCell(inst.VM), inst.SSH, inst.Service,
			inst.Keepalived, inst.HoldsAddress, inst.ConfigDrift, up,
		}
		for i, c := range cells {
			cells[i] = toonCell(c)
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(cells, ","))
	}

	fields := res.Fields
	if len(fields) == 0 {
		fields = DefaultStatFields
	}
	if len(res.Backends) == 0 {
		b.WriteString("backends: 0 server rows reported by any instance\n")
	} else {
		fmt.Fprintf(&b, "backendsFrom: %s\n", res.BackendsFrom)
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

// vmCell collapses the VM layer to one word: its power state when Terraform
// state was read, otherwise why it was not.
func vmCell(vm VMInfo) string {
	if vm.Source == "terraform" || vm.Source == "" {
		return vm.State
	}
	if vm.State == "not_found" {
		return vm.State
	}
	return vm.Source
}

// InstancePlan is one member's difference from the rendered config.
type InstancePlan struct {
	Instance
	// Drift is "true", "false", or Unknown when the deployed file was unread.
	Drift        string `json:"drift"`
	DeployedHash string `json:"deployedHash,omitempty"`
	Diff         string `json:"diff,omitempty"`
	DiffBytes    int    `json:"diffBytes"`
	Truncated    bool   `json:"truncated"`
	Error        string `json:"error,omitempty"`
}

// GroupPlanResult is the difference between the rendered config and the one
// installed on each instance.
type GroupPlanResult struct {
	VirtualAddress string         `json:"virtualAddress"`
	Drift          bool           `json:"drift"`
	Backends       int            `json:"backends"`
	RenderedHash   string         `json:"renderedHash"`
	Instances      []InstancePlan `json:"instances"`
	Notes          []string       `json:"notes,omitempty"`
	Failure        *Failure       `json:"error,omitempty"`
	Help           []string       `json:"-"`
}

// ReportGroupPlan renders a GroupPlanResult as TOON. Instances that would
// receive the same change share one diff rather than repeating it.
func ReportGroupPlan(res GroupPlanResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  virtualAddress: %s\n", orDash(res.VirtualAddress))
	fmt.Fprintf(&b, "  drift: %t\n", res.Drift)
	fmt.Fprintf(&b, "  backends: %d\n", res.Backends)
	fmt.Fprintf(&b, "  renderedHash: %s\n", shortHash(res.RenderedHash))

	if len(res.Instances) > 0 {
		fmt.Fprintf(&b, "instances[%d]{name,host,drift,deployedHash}:\n", len(res.Instances))
		for _, inst := range res.Instances {
			fmt.Fprintf(&b, "  %s,%s,%s,%s\n",
				toonCell(inst.Name), toonCell(inst.Host), toonCell(inst.Drift), shortHash(inst.DeployedHash))
		}
	}

	var drifted []InstancePlan
	for _, inst := range res.Instances {
		if inst.Drift == "true" {
			drifted = append(drifted, inst)
		}
	}

	switch {
	case len(drifted) > 0:
		b.WriteString("diffs:\n")
		printed := map[string]string{}
		for _, inst := range drifted {
			if first, ok := printed[inst.Diff]; ok {
				fmt.Fprintf(&b, "  %s: same as %s\n", inst.Label(), first)
				continue
			}
			printed[inst.Diff] = inst.Label()
			fmt.Fprintf(&b, "  %s: |\n", inst.Label())
			for _, line := range strings.Split(strings.TrimRight(inst.Diff, "\n"), "\n") {
				b.WriteString("    " + line + "\n")
			}
			if inst.Truncated {
				fmt.Fprintf(&b, "    ... (truncated, %d chars total — use --full)\n", inst.DiffBytes)
			}
		}
	case res.Failure == nil && len(res.Instances) > 0:
		b.WriteString("diff: 0 changes — every instance already matches current cluster state\n")
	}

	writeNotes(&b, res.Notes)
	writeFailure(&b, res.Failure)
	writeHelp(&b, res.Help)
	return b.String()
}

// InstanceApply is the outcome of the push on one member.
type InstanceApply struct {
	Instance
	Drift   bool   `json:"drift"`
	Changed bool   `json:"changed"`
	Error   string `json:"error,omitempty"`
}

// GroupApplyResult is the outcome of a config push across the group.
type GroupApplyResult struct {
	VirtualAddress string          `json:"virtualAddress"`
	Changed        bool            `json:"changed"`
	Drift          bool            `json:"drift"`
	Backends       int             `json:"backends"`
	RenderedHash   string          `json:"renderedHash"`
	DryRun         bool            `json:"dryRun"`
	Instances      []InstanceApply `json:"instances"`
	Notes          []string        `json:"notes,omitempty"`
	Failure        *Failure        `json:"error,omitempty"`
	Help           []string        `json:"-"`
}

// Conclude sets the aggregates and the verdict from the per-instance outcomes.
// A push that landed on some members and not others is named for what it
// leaves behind — instances serving different configs — rather than reported
// as a plain failure that reads as "nothing changed".
func (r *GroupApplyResult) Conclude() {
	var changed, failed []string
	for _, inst := range r.Instances {
		r.Drift = r.Drift || inst.Drift
		if inst.Changed {
			changed = append(changed, inst.Label())
		}
		if inst.Error != "" {
			failed = append(failed, inst.Label())
		}
	}
	r.Changed = len(changed) > 0

	if len(failed) == 0 || r.Failure != nil {
		return
	}
	if len(failed) == len(r.Instances) {
		r.Failure = &Failure{Code: "push_failed", Msg: fmt.Sprintf(
			"config push failed on every instance (%s); none was changed", strings.Join(failed, ", "))}
		return
	}
	r.Failure = &Failure{Code: "group_divergent", Msg: fmt.Sprintf(
		"%s did not take the config, so the instances are not known to be serving the same one — a failover onto it would serve stale backends",
		strings.Join(failed, ", "))}
}

// ReportGroupApply renders a GroupApplyResult as TOON.
func ReportGroupApply(res GroupApplyResult) string {
	var b strings.Builder
	b.WriteString("haproxy:\n")
	fmt.Fprintf(&b, "  virtualAddress: %s\n", orDash(res.VirtualAddress))
	fmt.Fprintf(&b, "  changed: %t\n", res.Changed)
	fmt.Fprintf(&b, "  drift: %t\n", res.Drift)
	fmt.Fprintf(&b, "  backends: %d\n", res.Backends)
	fmt.Fprintf(&b, "  renderedHash: %s\n", shortHash(res.RenderedHash))
	if res.DryRun {
		b.WriteString("  dryRun: true\n")
	}

	if len(res.Instances) > 0 {
		fmt.Fprintf(&b, "instances[%d]{name,host,drift,changed,error}:\n", len(res.Instances))
		for _, inst := range res.Instances {
			fmt.Fprintf(&b, "  %s,%s,%t,%t,%s\n",
				toonCell(inst.Name), toonCell(inst.Host), inst.Drift, inst.Changed, toonCell(inst.Error))
		}
	}
	if res.Failure == nil && !res.Drift {
		b.WriteString("result: 0 changes pushed — every instance was already current\n")
	}

	writeNotes(&b, res.Notes)
	writeFailure(&b, res.Failure)
	writeHelp(&b, res.Help)
	return b.String()
}
