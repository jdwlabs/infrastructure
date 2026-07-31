package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/terraform"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

// vmLookupTimeout bounds the read of Terraform state. It crosses the network to
// the remote backend, and the VM layer is one line of a health report — a slow
// backend degrades that line rather than hanging the whole command.
const vmLookupTimeout = 30 * time.Second

// HAProxyOptions carries the flags the haproxy command group accepts.
type HAProxyOptions struct {
	// Host overrides the push target. The rebuild runbook needs it: a
	// replacement load balancer is verified on a temporary address before DNS
	// or tfvars are changed to point at it.
	Host   string
	Fields []string
	Full   bool
	JSON   bool
	Out    io.Writer
}

// haproxyContext is everything the three commands share: resolved config, the
// deployed cluster state the config is rendered from, and the target address.
type haproxyContext struct {
	cfg      *types.Config
	stateMgr *state.Manager
	deployed *types.ClusterState
	host     string
}

// loadHAProxyContext resolves tfvars and cluster state. Both are read-only.
func (app *App) loadHAProxyContext(ctx context.Context, opts HAProxyOptions) (*haproxyContext, *haproxy.Failure) {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		return nil, &haproxy.Failure{
			Code: "tfvars_not_found",
			Msg:  fmt.Sprintf("could not locate terraform.tfvars: %v", err),
		}
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		return nil, &haproxy.Failure{
			Code: "tfvars_unreadable",
			Msg:  fmt.Sprintf("could not read %s: %v", cfg.TerraformTFVars, err),
		}
	}

	host := opts.Host
	if host == "" && cfg.HAProxyIP != nil {
		host = cfg.HAProxyIP.String()
	}
	if host == "" {
		return nil, &haproxy.Failure{
			Code: "haproxy_host_unset",
			Msg:  "no load-balancer address: haproxy_ip is absent from tfvars and --host was not given",
		}
	}
	if net.ParseIP(host) == nil {
		return nil, &haproxy.Failure{
			Code: "haproxy_host_invalid",
			Msg:  fmt.Sprintf("%q is not an IP address", host),
		}
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		return nil, &haproxy.Failure{
			Code: "state_unreadable",
			Msg:  fmt.Sprintf("could not read cluster state: %v", err),
		}
	}

	return &haproxyContext{cfg: cfg, stateMgr: stateMgr, deployed: deployed, host: host}, nil
}

// renderConfig builds the config the generator would push for current state.
func (hc *haproxyContext) renderConfig() (string, int, *haproxy.Failure) {
	if len(hc.deployed.ControlPlanes) == 0 {
		return "", 0, &haproxy.Failure{
			Code: "no_control_planes",
			Msg:  "cluster state records no control planes, so no backends can be rendered",
		}
	}

	// The generator binds frontends to the address it is told about. Rendering
	// for an override host without carrying it through would produce a config
	// that binds the production address on a VM that does not hold it.
	cfg := *hc.cfg
	cfg.HAProxyIP = net.ParseIP(hc.host)

	haConfig := haproxy.ConfigFromClusterState(&cfg, hc.deployed)
	rendered, err := haConfig.Generate()
	if err != nil {
		return "", 0, &haproxy.Failure{
			Code: "render_failed",
			Msg:  fmt.Sprintf("could not render config: %v", err),
		}
	}
	return rendered, len(haConfig.ControlPlanes) + len(haConfig.IngressNodes), nil
}

// client builds the SSH client for the resolved host.
func (app *App) haproxyClient(hc *haproxyContext) (*haproxy.Client, *haproxy.Failure) {
	cfg := *hc.cfg
	cfg.HAProxyIP = net.ParseIP(hc.host)

	client := app.createHAProxyClient(&cfg)
	if client == nil {
		return nil, &haproxy.Failure{
			Code: "ssh_auth_unconfigured",
			Msg:  "no SSH auth available: pass --haproxy-ssh-key or --ssh-key, or run an SSH agent",
		}
	}
	return client, nil
}

// RunHAProxyStatus reports health across every layer between here and the
// backends. It reads only: nothing in this path writes to the load balancer,
// to Proxmox, or to Terraform state.
func (app *App) RunHAProxyStatus(ctx context.Context, opts HAProxyOptions) error {
	res := haproxy.StatusResult{
		SSH:         haproxy.Unknown,
		Service:     haproxy.Unknown,
		ConfigDrift: haproxy.Unknown,
		VM:          haproxy.VMInfo{State: haproxy.Unknown, Source: haproxy.Unknown},
	}

	hc, failure := app.loadHAProxyContext(ctx, opts)
	if failure != nil {
		res.Host = opts.Host
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "status", haproxy.ReportStatus(res), res)
	}
	res.Host = hc.host

	vm, vmNotes := app.resolveHAProxyVM(ctx, hc)
	res.VM = vm
	res.Notes = append(res.Notes, vmNotes...)

	client, failure := app.haproxyClient(hc)
	if failure != nil {
		res.SSH = "unavailable"
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "status", haproxy.ReportStatus(res), res)
	}

	if err := client.CheckConnectivity(); err != nil {
		res.SSH = "unreachable"
		res.Failure = &haproxy.Failure{
			Code: "ssh_unreachable",
			Msg:  fmt.Sprintf("SSH to %s@%s failed: %v", hc.cfg.HAProxyLoginUser, hc.host, err),
		}
		res.Help = helpForFailure(res.Failure)
		res.Help = append(res.Help, vmHelp(vm)...)
		return app.emitHAProxy(opts, "status", haproxy.ReportStatus(res), res)
	}
	res.SSH = "ok"

	if serviceState, err := client.ServiceState(ctx); err != nil {
		res.Notes = append(res.Notes, "service state unread: "+err.Error())
	} else {
		res.Service = serviceState
	}

	stats, err := client.Stats(ctx)
	if err != nil {
		res.Notes = append(res.Notes,
			"backend health unread: "+err.Error()+" (needs socat or nc on the host)")
	} else {
		res.Backends = stats
	}

	fields, fieldErr := resolveFields(opts, stats)
	if fieldErr != nil {
		res.Failure = fieldErr
		res.Help = helpForFailure(fieldErr)
		return app.emitHAProxy(opts, "status", haproxy.ReportStatus(res), res)
	}
	res.Fields = fields

	res.ConfigDrift, res.Notes = app.driftFor(ctx, hc, client, res.Notes)
	res.Help = statusHelp(res, vm)

	if opts.JSON {
		return haproxy.EmitJSON(app.haproxyOut(opts), "status", struct {
			haproxy.StatusResult
			Backends   []map[string]string `json:"backends"`
			BackendsUp int                 `json:"backendsUp"`
		}{
			StatusResult: res,
			Backends:     haproxy.BackendsJSON(res.Backends, fields),
			BackendsUp:   haproxy.UpCount(res.Backends),
		})
	}
	_, err = fmt.Fprint(app.haproxyOut(opts), haproxy.ReportStatus(res))
	return err
}

// driftFor compares the rendered config against the file on the host. When the
// live file cannot be read the answer stays "unknown": the hash recorded in
// cluster state only knows what talops last pushed, so treating it as the
// deployed config would report a hand-edited host as converged.
func (app *App) driftFor(
	ctx context.Context,
	hc *haproxyContext,
	client *haproxy.Client,
	notes []string,
) (string, []string) {
	rendered, _, failure := hc.renderConfig()
	if failure != nil {
		return haproxy.Unknown, append(notes, "config drift unchecked: "+failure.Msg)
	}

	deployed, err := client.DeployedConfig(ctx)
	if err != nil {
		notes = append(notes, "config drift unchecked: "+err.Error())
		if hc.deployed.HAProxyConfigHash != "" {
			notes = append(notes, fmt.Sprintf(
				"last config pushed by talops was %s; that record cannot see a change made on the host",
				hc.deployed.HAProxyConfigHash[:12]))
		}
		return haproxy.Unknown, notes
	}

	if haproxy.Diff(deployed, rendered) == "" {
		return "false", notes
	}
	return "true", notes
}

// RunHAProxyPlan previews the config change without touching the host. It exits
// successfully whether or not drift exists — drift is the answer, not a failure.
func (app *App) RunHAProxyPlan(ctx context.Context, opts HAProxyOptions) error {
	res := haproxy.PlanResult{}

	hc, failure := app.loadHAProxyContext(ctx, opts)
	if failure != nil {
		res.Host = opts.Host
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "plan", haproxy.ReportPlan(res), res)
	}
	res.Host = hc.host

	rendered, backends, failure := hc.renderConfig()
	if failure != nil {
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "plan", haproxy.ReportPlan(res), res)
	}
	res.Backends = backends
	res.RenderedHash = haproxy.Hash(rendered)

	client, failure := app.haproxyClient(hc)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "plan", haproxy.ReportPlan(res), res)
	}

	deployed, err := client.DeployedConfig(ctx)
	if err != nil {
		res.Failure = &haproxy.Failure{
			Code: "config_unreadable",
			Msg:  fmt.Sprintf("could not read %s on %s: %v", haproxy.ConfigPath, hc.host, err),
		}
		res.Help = helpForFailure(res.Failure)
		return app.emitHAProxy(opts, "plan", haproxy.ReportPlan(res), res)
	}
	res.DeployedHash = haproxy.Hash(deployed)

	diff := haproxy.Diff(deployed, rendered)
	res.Drift = diff != ""
	res.DiffBytes = len(diff)
	if opts.Full {
		res.Diff = diff
	} else {
		res.Diff, res.Truncated = haproxy.Truncate(diff, haproxy.DiffLimit)
	}
	res.Help = planHelp(res)

	if opts.JSON {
		return haproxy.EmitJSON(app.haproxyOut(opts), "plan", struct {
			haproxy.PlanResult
			Diff string `json:"diff"`
		}{PlanResult: res, Diff: res.Diff})
	}
	_, err = fmt.Fprint(app.haproxyOut(opts), haproxy.ReportPlan(res))
	return err
}

// RunHAProxyApply pushes the rendered config through the same validated,
// auto-rollback path reconcile uses. It touches the load balancer's config file
// only — never Proxmox, never Terraform state, never the VM's lifecycle.
func (app *App) RunHAProxyApply(ctx context.Context, opts HAProxyOptions) error {
	res := haproxy.ApplyResult{DryRun: app.Cfg.DryRun}
	out := app.haproxyOut(opts)

	hc, failure := app.loadHAProxyContext(ctx, opts)
	if failure != nil {
		res.Host = opts.Host
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}
	res.Host = hc.host

	rendered, backends, failure := hc.renderConfig()
	if failure != nil {
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}
	res.Backends = backends
	res.RenderedHash = haproxy.Hash(rendered)
	if opts.JSON {
		if err := haproxy.EmitJSON(out, "rendered", res); err != nil {
			return err
		}
	}

	client, failure := app.haproxyClient(hc)
	if failure != nil {
		res.Failure = failure
		res.Help = helpForFailure(failure)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}

	deployed, err := client.DeployedConfig(ctx)
	if err != nil {
		// An unreadable file is not proof of no drift. Push rather than skip:
		// the install path validates and rolls itself back, so the safe default
		// when drift is unknown is to converge.
		res.Notes = append(res.Notes,
			"deployed config unread ("+err.Error()+"); pushing rather than assuming convergence")
		res.Drift = true
	} else {
		res.Drift = haproxy.Diff(deployed, rendered) != ""
	}

	if !res.Drift {
		res.Help = applyHelp(res)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}

	if app.Cfg.DryRun {
		res.Notes = append(res.Notes, "dry run: config drift exists and nothing was pushed")
		res.Help = applyHelp(res)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}

	if opts.JSON {
		if err := haproxy.EmitJSON(out, "pushing", res); err != nil {
			return err
		}
	}

	if err := client.Update(ctx, rendered); err != nil {
		res.Failure = &haproxy.Failure{
			Code: "push_failed",
			Msg:  fmt.Sprintf("config push to %s failed: %v", hc.host, err),
		}
		res.Help = helpForFailure(res.Failure)
		return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
	}

	res.Changed = true
	app.recordHAProxyHash(ctx, hc.stateMgr, hc.deployed, res.RenderedHash)
	res.Help = applyHelp(res)
	return app.emitHAProxy(opts, "apply", haproxy.ReportApply(res), res)
}

// recordHAProxyHash persists the fingerprint of the config just pushed. A
// failure here is not a failed push: the config is already installed and
// reloaded, and reporting otherwise would invite a pointless retry.
func (app *App) recordHAProxyHash(
	ctx context.Context,
	stateMgr *state.Manager,
	deployed *types.ClusterState,
	hash string,
) {
	deployed.HAProxyConfigHash = hash
	if err := stateMgr.Save(ctx, deployed); err != nil {
		app.Logger.Warn("could not record HAProxy config hash in cluster state: " + err.Error())
	}
}

// resolveHAProxyVM identifies the VM behind the target address. Both reads are
// read-only: the tfvars declaration, and `terraform show -json`, which inspects
// state without locking or planning it.
func (app *App) resolveHAProxyVM(ctx context.Context, hc *haproxyContext) (haproxy.VMInfo, []string) {
	var declared *types.HAProxyVM
	for i := range hc.cfg.HAProxyVMs {
		if hc.cfg.HAProxyVMs[i].IP == hc.host {
			declared = &hc.cfg.HAProxyVMs[i]
			break
		}
	}

	if declared == nil {
		return haproxy.VMInfo{State: haproxy.Unknown, Source: "unmanaged"}, []string{
			fmt.Sprintf("%s matches no haproxy_vms entry — this load balancer is not reproducible from this repo", hc.host),
		}
	}

	vm := haproxy.VMInfo{
		Name:   declared.Name,
		VMID:   declared.VMID,
		Node:   declared.Node,
		State:  haproxy.Unknown,
		Source: "declared",
	}

	tfDir, err := app.ResolveTerraformDir()
	if err != nil {
		return vm, []string{"VM state unread: " + err.Error()}
	}
	if _, err := terraform.LookPath(); err != nil {
		return vm, []string{"VM state unread: terraform is not on PATH"}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, vmLookupTimeout)
	defer cancel()

	stateOut, err := terraform.NewRunner(tfDir, app.Logger).ShowStateJSON(lookupCtx)
	if err != nil {
		return vm, []string{"VM state unread: " + err.Error()}
	}

	found := findHAProxyVMState(stateOut, declared.VMID)
	if found == "" {
		vm.State = "not_found"
		return vm, []string{fmt.Sprintf(
			"VMID %d is declared in tfvars but absent from Terraform state — it has not been applied",
			declared.VMID)}
	}
	vm.State = found
	vm.Source = "terraform"
	return vm, nil
}

// findHAProxyVMState locates the load-balancer VM in Terraform state and
// returns "running" or "stopped", or "" when it is not in state at all.
func findHAProxyVMState(stateOut *terraform.StateOutput, vmid int) string {
	if stateOut == nil || stateOut.Values == nil || stateOut.Values.RootModule == nil {
		return ""
	}
	for _, res := range stateOut.Values.RootModule.Resources {
		if res.Type != "proxmox_virtual_environment_vm" || res.Name != "haproxy" {
			continue
		}
		id, ok := res.Values["vm_id"].(float64)
		if !ok || int(id) != vmid {
			continue
		}
		if started, ok := res.Values["started"].(bool); ok && !started {
			return "stopped"
		}
		return "running"
	}
	return ""
}

func resolveFields(opts HAProxyOptions, stats []haproxy.ServerStat) ([]string, *haproxy.Failure) {
	if opts.Full {
		return haproxy.AllStatFields(stats), nil
	}
	fields, err := haproxy.ResolveStatFields(opts.Fields, stats)
	if err != nil {
		return nil, &haproxy.Failure{Code: "unknown_field", Msg: err.Error()}
	}
	return fields, nil
}

func (app *App) haproxyOut(opts HAProxyOptions) io.Writer {
	if opts.Out != nil {
		return opts.Out
	}
	return io.Discard
}

// emitHAProxy writes the result in whichever format was asked for, and returns
// the failure so the process exit status matches what the caller was told.
func (app *App) emitHAProxy(opts HAProxyOptions, event, toon string, payload any) error {
	if opts.JSON {
		if err := haproxy.EmitJSON(app.haproxyOut(opts), event, payload); err != nil {
			return err
		}
	} else if _, err := fmt.Fprint(app.haproxyOut(opts), toon); err != nil {
		return err
	}

	switch res := payload.(type) {
	case haproxy.StatusResult:
		return failureOf(res.Failure)
	case haproxy.PlanResult:
		return failureOf(res.Failure)
	case haproxy.ApplyResult:
		return failureOf(res.Failure)
	}
	return nil
}

func failureOf(f *haproxy.Failure) error {
	if f == nil {
		return nil
	}
	return f
}
