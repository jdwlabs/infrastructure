package app

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
)

// groupMember is what the group commands need from one instance. It exists so
// the per-instance bookkeeping — which is where a pair can end up silently
// divergent — is exercised without an SSH server per member.
type groupMember interface {
	CheckConnectivity() error
	ServiceState(ctx context.Context) (string, error)
	KeepalivedState(ctx context.Context) (string, error)
	HoldsAddress(ctx context.Context, addr net.IP) (bool, error)
	Stats(ctx context.Context) ([]haproxy.ServerStat, error)
	DeployedConfig(ctx context.Context) (string, error)
	Update(ctx context.Context, config string) error
}

type connectMember func(haproxy.Instance) (groupMember, *haproxy.Failure)

func (app *App) memberConnector(hc *haproxyContext) connectMember {
	return func(inst haproxy.Instance) (groupMember, *haproxy.Failure) {
		client, failure := app.haproxyClientFor(hc, inst.Host)
		if failure != nil {
			return nil, failure
		}
		return client, nil
	}
}

func (app *App) runHAProxyGroupStatus(ctx context.Context, opts HAProxyOptions, hc *haproxyContext) error {
	readState := app.terraformStateReader(ctx)
	res := inspectGroup(ctx, hc, app.memberConnector(hc),
		func(host string) (haproxy.VMInfo, []string) { return app.describeHAProxyVM(hc.cfg, host, readState) })

	fields, fieldErr := resolveFields(opts, res.Backends)
	if fieldErr != nil {
		res.Failure = fieldErr
		res.Help = helpForFailure(fieldErr)
		return app.emitHAProxy(opts, "status", haproxy.ReportGroupStatus(res), res)
	}
	res.Fields = fields
	res.Help = groupStatusHelp(res)

	if opts.JSON {
		if err := haproxy.EmitJSON(app.haproxyOut(opts), "status", struct {
			haproxy.GroupStatusResult
			Backends   []map[string]string `json:"backends"`
			BackendsUp int                 `json:"backendsUp"`
		}{
			GroupStatusResult: res,
			Backends:          haproxy.BackendsJSON(res.Backends, fields),
			BackendsUp:        haproxy.UpCount(res.Backends),
		}); err != nil {
			return err
		}
		return failureOf(res.Failure)
	}
	return app.emitHAProxy(opts, "status", haproxy.ReportGroupStatus(res), res)
}

// inspectGroup reads every layer of every instance. One instance being
// unreadable never stops the others being read: the report of a degraded
// group is the report that matters most.
func inspectGroup(
	ctx context.Context,
	hc *haproxyContext,
	connect connectMember,
	describeVM func(host string) (haproxy.VMInfo, []string),
) haproxy.GroupStatusResult {
	res := haproxy.GroupStatusResult{VirtualAddress: hc.group.BindAddress.String()}
	note := func(inst haproxy.Instance, msg string) {
		res.Notes = appendUnique(res.Notes, inst.Label()+": "+msg)
	}

	rendered, _, renderFailure := hc.renderConfig()
	if renderFailure != nil {
		res.Notes = append(res.Notes, "config drift unchecked: "+renderFailure.Msg)
	}

	for _, inst := range hc.group.Instances {
		st := haproxy.InstanceStatus{
			Instance:     inst,
			SSH:          haproxy.Unknown,
			Service:      haproxy.Unknown,
			Keepalived:   haproxy.Unknown,
			HoldsAddress: haproxy.Unknown,
			ConfigDrift:  haproxy.Unknown,
		}

		vm, vmNotes := describeVM(inst.Host)
		st.VM = vm
		for _, n := range vmNotes {
			// The state read is shared, so its failure is one fact, not one
			// per instance.
			if strings.HasPrefix(n, "VM state unread") {
				res.Notes = appendUnique(res.Notes, n)
				continue
			}
			note(inst, n)
		}

		member, failure := connect(inst)
		if failure != nil {
			st.SSH = "unavailable"
			if res.Failure == nil {
				res.Failure = failure
			}
			res.Instances = append(res.Instances, st)
			continue
		}
		if err := member.CheckConnectivity(); err != nil {
			st.SSH = "unreachable"
			note(inst, fmt.Sprintf("SSH to %s@%s failed: %v", hc.cfg.HAProxyLoginUser, inst.Host, err))
			res.Instances = append(res.Instances, st)
			continue
		}
		st.SSH = "ok"

		if state, err := member.ServiceState(ctx); err != nil {
			note(inst, "service state unread: "+err.Error())
		} else {
			st.Service = state
		}
		if state, err := member.KeepalivedState(ctx); err != nil {
			note(inst, "keepalived state unread: "+err.Error())
		} else {
			st.Keepalived = state
		}
		if holds, err := member.HoldsAddress(ctx, hc.group.BindAddress); err != nil {
			note(inst, "virtual address unchecked: "+err.Error())
		} else {
			st.HoldsAddress = fmt.Sprintf("%t", holds)
		}
		if stats, err := member.Stats(ctx); err != nil {
			note(inst, "backend health unread: "+err.Error()+" (needs socat or nc on the host)")
		} else {
			st.Backends = stats
			st.BackendsUp = haproxy.UpCount(stats)
			st.BackendsTotal = len(stats)
		}
		if renderFailure == nil {
			if deployed, err := member.DeployedConfig(ctx); err != nil {
				note(inst, "config drift unchecked: "+err.Error())
			} else {
				st.ConfigDrift = fmt.Sprintf("%t", haproxy.Diff(deployed, rendered) != "")
			}
		}

		res.Instances = append(res.Instances, st)
	}

	res.Assess()
	return res
}

func (app *App) runHAProxyGroupPlan(ctx context.Context, opts HAProxyOptions, hc *haproxyContext) error {
	res := planGroup(ctx, hc, app.memberConnector(hc), opts.Full)
	res.Help = groupPlanHelp(res)
	return app.emitHAProxy(opts, "plan", haproxy.ReportGroupPlan(res), res)
}

// planGroup diffs the rendered config against each instance's deployed file.
func planGroup(ctx context.Context, hc *haproxyContext, connect connectMember, full bool) haproxy.GroupPlanResult {
	res := haproxy.GroupPlanResult{VirtualAddress: hc.group.BindAddress.String()}

	rendered, backends, failure := hc.renderConfig()
	if failure != nil {
		res.Failure = failure
		return res
	}
	res.Backends = backends
	res.RenderedHash = haproxy.Hash(rendered)

	var unread []string
	for _, inst := range hc.group.Instances {
		plan := haproxy.InstancePlan{Instance: inst, Drift: haproxy.Unknown}

		member, failure := connect(inst)
		if failure != nil {
			res.Failure = failure
			return res
		}
		deployed, err := member.DeployedConfig(ctx)
		if err != nil {
			plan.Error = err.Error()
			unread = append(unread, fmt.Sprintf("%s (%v)", inst.Label(), err))
			res.Instances = append(res.Instances, plan)
			continue
		}

		plan.DeployedHash = haproxy.Hash(deployed)
		diff := haproxy.Diff(deployed, rendered)
		plan.Drift = fmt.Sprintf("%t", diff != "")
		plan.DiffBytes = len(diff)
		plan.Diff = diff
		if !full {
			plan.Diff, plan.Truncated = haproxy.Truncate(diff, haproxy.DiffLimit)
		}
		res.Drift = res.Drift || diff != ""
		res.Instances = append(res.Instances, plan)
	}

	if len(unread) > 0 {
		res.Failure = &haproxy.Failure{
			Code: "config_unreadable",
			Msg: fmt.Sprintf("could not read %s on %s — drift there is unknown, not absent",
				haproxy.ConfigPath, strings.Join(unread, "; ")),
		}
	}
	return res
}

func (app *App) runHAProxyGroupApply(ctx context.Context, opts HAProxyOptions, hc *haproxyContext) error {
	out := app.haproxyOut(opts)

	var emitErr error
	res := applyGroup(ctx, hc, app.memberConnector(hc), app.Cfg.DryRun, func(event string, sofar haproxy.GroupApplyResult) {
		if opts.JSON && emitErr == nil {
			emitErr = haproxy.EmitJSON(out, event, sofar)
		}
	})
	if emitErr != nil {
		return emitErr
	}

	// The recorded hash claims "this is what talops last pushed". With an
	// instance left behind that claim is only true of part of the group.
	if res.Changed && res.Failure == nil {
		app.recordHAProxyHash(ctx, hc.stateMgr, hc.deployed, res.RenderedHash)
	}
	res.Help = groupApplyHelp(res)
	return app.emitHAProxy(opts, "apply", haproxy.ReportGroupApply(res), res)
}

// applyGroup converges every instance that differs from the rendered config.
// It never stops at a failed instance — the one still reachable is the one
// serving — and the result names what that leaves behind.
func applyGroup(
	ctx context.Context,
	hc *haproxyContext,
	connect connectMember,
	dryRun bool,
	progress func(event string, sofar haproxy.GroupApplyResult),
) haproxy.GroupApplyResult {
	res := haproxy.GroupApplyResult{VirtualAddress: hc.group.BindAddress.String(), DryRun: dryRun}

	rendered, backends, failure := hc.renderConfig()
	if failure != nil {
		res.Failure = failure
		return res
	}
	res.Backends = backends
	res.RenderedHash = haproxy.Hash(rendered)
	progress("rendered", res)

	for _, inst := range hc.group.Instances {
		outcome := haproxy.InstanceApply{Instance: inst}

		member, failure := connect(inst)
		if failure != nil {
			res.Failure = failure
			return res
		}

		deployed, err := member.DeployedConfig(ctx)
		if err != nil {
			// An unreadable file is not proof of no drift. Push rather than
			// skip: the install path validates and rolls itself back, so the
			// safe default when drift is unknown is to converge.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: deployed config unread (%v); pushing rather than assuming convergence", inst.Label(), err))
			outcome.Drift = true
		} else {
			outcome.Drift = haproxy.Diff(deployed, rendered) != ""
		}

		if outcome.Drift && !dryRun {
			progress("pushing", res)
			if err := member.Update(ctx, rendered); err != nil {
				outcome.Error = err.Error()
			} else {
				outcome.Changed = true
			}
		}
		res.Instances = append(res.Instances, outcome)
	}

	res.Conclude()
	if dryRun && res.Drift {
		res.Notes = append(res.Notes, "dry run: config drift exists and nothing was pushed")
	}
	return res
}

func groupStatusHelp(res haproxy.GroupStatusResult) []string {
	help := helpForFailure(res.Failure)

	drift := false
	down := false
	for _, inst := range res.Instances {
		drift = drift || inst.ConfigDrift != "false"
		down = down || (inst.BackendsTotal > 0 && inst.BackendsUp < inst.BackendsTotal)
	}
	if drift {
		help = append(help, "talops haproxy plan  # per-instance diff against the rendered config")
	}
	if down {
		help = append(help,
			"A backend is down: talosctl -n <cp-ip> service etcd status  # check the node itself, not the load balancer")
	}
	return help
}

func groupPlanHelp(res haproxy.GroupPlanResult) []string {
	if res.Failure != nil {
		return helpForFailure(res.Failure)
	}
	if !res.Drift {
		return []string{"talops haproxy status  # which instance holds the virtual address, and backend health"}
	}
	help := []string{"talops haproxy apply  # push to every drifting instance (validated, rolled back on failure)"}
	for _, inst := range res.Instances {
		if inst.Truncated {
			return append(help, "talops haproxy plan --full  # the untruncated diffs")
		}
	}
	return help
}

func groupApplyHelp(res haproxy.GroupApplyResult) []string {
	if res.Failure != nil {
		return helpForFailure(res.Failure)
	}
	if res.DryRun && res.Drift {
		return []string{"talops haproxy apply  # without --dry-run, to push the change"}
	}
	if res.Changed {
		return []string{"talops haproxy status  # confirm every backend came back up on every instance"}
	}
	return nil
}

func appendUnique(list []string, item string) []string {
	if slices.Contains(list, item) {
		return list
	}
	return append(list, item)
}

// Kept as a type assertion so a change to the SSH client that drops a method
// the group commands rely on fails here, at compile time, rather than at the
// first status call against a real pair.
var _ groupMember = (*haproxy.Client)(nil)
