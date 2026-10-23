package app

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/kubectl"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/talos"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

// Variables rather than constants so tests can run the waits without sleeping.
var (
	upgradeNodeTimeout  = 10 * time.Minute
	upgradePollInterval = 10 * time.Second
)

// upgradeNodeEntry carries per-node data for the upgrade sequence.
type upgradeNodeEntry struct {
	VMID types.VMID
	IP   net.IP
	Role types.Role
}

// upgradeDeps abstracts the Talos and kubectl API calls made during upgrade
// so the orchestration logic can be unit-tested without a real cluster.
type upgradeDeps interface {
	NodeVersion(ctx context.Context, ip net.IP) (string, error)
	Upgrade(ctx context.Context, ip net.IP, image string) error
	WaitForEtcdMembers(ctx context.Context, ip net.IP, expected int, d time.Duration) error
	GetNodeNameByIP(ctx context.Context, ip net.IP) (string, error)
	WaitForNodeReady(ctx context.Context, nodeName string, d time.Duration) error
	CordonNode(ctx context.Context, nodeName string) error
	UncordonNode(ctx context.Context, nodeName string) error
	WaitForLonghornVolumesHealthy(ctx context.Context, d time.Duration) error
}

// realUpgradeDeps wires real talos.Client + kubectl.Client to the upgradeDeps interface.
type realUpgradeDeps struct {
	talos   *talos.Client
	kubectl *kubectl.Client
}

func (r *realUpgradeDeps) NodeVersion(ctx context.Context, ip net.IP) (string, error) {
	return r.talos.NodeVersion(ctx, ip)
}

func (r *realUpgradeDeps) Upgrade(ctx context.Context, ip net.IP, image string) error {
	return r.talos.Upgrade(ctx, ip, image)
}

func (r *realUpgradeDeps) WaitForEtcdMembers(ctx context.Context, ip net.IP, expected int, d time.Duration) error {
	return r.talos.WaitForEtcdMembers(ctx, ip, expected, d)
}

func (r *realUpgradeDeps) GetNodeNameByIP(ctx context.Context, ip net.IP) (string, error) {
	return r.kubectl.GetNodeNameByIP(ctx, ip)
}

func (r *realUpgradeDeps) WaitForNodeReady(ctx context.Context, nodeName string, d time.Duration) error {
	return r.kubectl.WaitForNodeReady(ctx, nodeName, d)
}

func (r *realUpgradeDeps) CordonNode(ctx context.Context, nodeName string) error {
	return r.kubectl.CordonNode(ctx, nodeName)
}

func (r *realUpgradeDeps) UncordonNode(ctx context.Context, nodeName string) error {
	return r.kubectl.UncordonNode(ctx, nodeName)
}

func (r *realUpgradeDeps) WaitForLonghornVolumesHealthy(ctx context.Context, d time.Duration) error {
	return r.kubectl.WaitForLonghornVolumesHealthy(ctx, d)
}

// RunUpgrade is the entry point for `talops upgrade`.
func (app *App) RunUpgrade(ctx context.Context, nodesFlag, imageFlag string) error {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Warn("could not locate terraform.tfvars", zap.Error(err))
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Warn("could not load terraform extras", zap.Error(err))
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		return fmt.Errorf("load deployed state: %w", err)
	}
	if !deployed.BootstrapCompleted {
		return fmt.Errorf("cluster has not completed bootstrap — run 'talops reconcile' first")
	}

	targetImage := cfg.InstallerImage
	if imageFlag != "" {
		targetImage = imageFlag
	}
	if targetImage == "" {
		return fmt.Errorf("target image is required: set installer_image in tfvars or pass --image")
	}

	var nodeFilter []net.IP
	if nodesFlag != "" {
		nodeFilter, err = parseNodeIPs(nodesFlag)
		if err != nil {
			return fmt.Errorf("parse --nodes flag: %w", err)
		}
	}

	talosClient := talos.NewClient(cfg)
	talosClient.SetLogger(app.Logger)
	if app.Session != nil && app.Session.AuditLog != nil {
		talosClient.SetAuditLogger(app.Session.AuditLog)
	}
	if err := talosClient.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize talos client: %w", err)
	}

	k8sClient := kubectl.NewClient(app.Logger)
	k8sClient.SetContext(cfg.ClusterName)
	if app.Session != nil && app.Session.AuditLog != nil {
		k8sClient.SetAuditLogger(app.Session.AuditLog)
	}

	deps := &realUpgradeDeps{talos: talosClient, kubectl: k8sClient}
	return app.runUpgradeWithState(ctx, deployed, nodeFilter, targetImage, deps)
}

// runUpgradeWithState contains the upgrade orchestration logic. The deps parameter
// allows injecting test doubles without touching the real cluster.
func (app *App) runUpgradeWithState(ctx context.Context, deployed *types.ClusterState, nodeFilter []net.IP, targetImage string, deps upgradeDeps) error {
	cfg := app.Cfg

	plan := buildUpgradePlan(deployed, nodeFilter)
	if len(plan) == 0 {
		app.Logger.Info("no nodes to upgrade (empty plan)")
		return nil
	}

	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("UPGRADE-START", fmt.Sprintf("cluster=%s image=%s nodes=%d dry_run=%v",
			cfg.ClusterName, targetImage, len(plan), cfg.DryRun))
	}

	app.printUpgradePlan(plan, targetImage, deps, ctx)

	if cfg.PlanMode || cfg.DryRun {
		app.Logger.Info("plan mode — exiting without changes")
		return nil
	}

	if !cfg.AutoApprove && app.Session != nil {
		if !app.PromptConfirm(fmt.Sprintf("Upgrade %d node(s) to %s? [y/N]: ", len(plan), targetImage)) {
			return nil
		}
	}

	for _, entry := range plan {
		if err := app.upgradeOneNode(ctx, entry, targetImage, deployed, deps); err != nil {
			if app.Session != nil && app.Session.AuditLog != nil {
				app.Session.AuditLog.WriteEntry("UPGRADE-END", fmt.Sprintf("cluster=%s status=failed node=%s error=%v",
					cfg.ClusterName, entry.IP, err))
			}
			return fmt.Errorf("upgrade node %s (VMID %d) failed — use `talosctl rollback` on that node if needed: %w",
				entry.IP, entry.VMID, err)
		}
	}

	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("UPGRADE-END", fmt.Sprintf("cluster=%s status=success nodes=%d image=%s",
			cfg.ClusterName, len(plan), targetImage))
	}
	app.Logger.Info("upgrade complete", zap.Int("nodes", len(plan)), zap.String("image", targetImage))
	return nil
}

// upgradeOneNode runs the full upgrade sequence for a single node.
// Returns an error on failure; the caller must abort the run.
func (app *App) upgradeOneNode(ctx context.Context, entry upgradeNodeEntry, targetImage string, deployed *types.ClusterState, deps upgradeDeps) error {
	log := app.Logger.With(
		zap.Int("vmid", int(entry.VMID)),
		zap.String("ip", entry.IP.String()),
		zap.String("role", string(entry.Role)),
	)

	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("UPGRADE-NODE", fmt.Sprintf("vmid=%d ip=%s role=%s image=%s",
			entry.VMID, entry.IP, entry.Role, targetImage))
	}

	// Check current version — skip if already at target
	currentVersion, err := deps.NodeVersion(ctx, entry.IP)
	if err != nil {
		log.Warn("could not query node version, proceeding with upgrade", zap.Error(err))
	} else if versionMatchesTarget(currentVersion, targetImage) {
		log.Info("node already at target version, skipping", zap.String("version", currentVersion))
		return nil
	} else {
		log.Info("upgrading node",
			zap.String("current_version", currentVersion),
			zap.String("target_image", targetImage))
	}

	// Every gate after the reboot needs the Kubernetes node name, so resolve it
	// while nothing has changed yet: failing here aborts with the node untouched.
	nodeName, err := deps.GetNodeNameByIP(ctx, entry.IP)
	if err != nil {
		return fmt.Errorf("resolve kubernetes node name for %s: %w", entry.IP, err)
	}

	// Workers are cordoned so no new workloads land on them before the reboot.
	if entry.Role == types.RoleWorker {
		if err := deps.CordonNode(ctx, nodeName); err != nil {
			return fmt.Errorf("cordon worker %s: %w", nodeName, err)
		}
	}

	if err := deps.Upgrade(ctx, entry.IP, targetImage); err != nil {
		if entry.Role == types.RoleWorker {
			_ = deps.UncordonNode(ctx, nodeName)
		}
		return fmt.Errorf("trigger upgrade: %w", err)
	}

	log.Info("upgrade triggered, waiting for node to reboot and become ready")

	// The upgrade call returns before the node reboots, and until it does the
	// API, the node's Ready condition and Longhorn all still look healthy. Only
	// the node reporting the target tag proves it rebooted into the new image.
	if err := waitForNodeVersion(ctx, deps, entry.IP, targetImage, upgradeNodeTimeout); err != nil {
		return err
	}

	// Control planes additionally need etcd to recover. A member list served
	// by the upgraded node itself needs its etcd up and in quorum; a peer would
	// answer while this member is still down.
	if entry.Role == types.RoleControlPlane {
		if err := deps.WaitForEtcdMembers(ctx, entry.IP, len(deployed.ControlPlanes), upgradeNodeTimeout); err != nil {
			return fmt.Errorf("etcd not healthy after upgrading %s: %w", entry.IP, err)
		}
	}

	if err := deps.WaitForNodeReady(ctx, nodeName, upgradeNodeTimeout); err != nil {
		return fmt.Errorf("node %s not Ready after upgrade: %w", nodeName, err)
	}

	// Workers: uncordon and then wait for Longhorn to heal before the next worker.
	if entry.Role == types.RoleWorker {
		if err := deps.UncordonNode(ctx, nodeName); err != nil {
			return fmt.Errorf("uncordon worker %s: %w", nodeName, err)
		}
		// Give Longhorn time to replicate data back before stressing the next node.
		if err := deps.WaitForLonghornVolumesHealthy(ctx, upgradeNodeTimeout); err != nil {
			return fmt.Errorf("longhorn volumes degraded after upgrading %s: %w", entry.IP, err)
		}
	}

	log.Info("node upgrade complete")
	return nil
}

// waitForNodeVersion polls the node until it reports the tag of targetImage.
// Errors while polling are expected (the node is rebooting) and only the
// deadline ends the wait.
func waitForNodeVersion(ctx context.Context, deps upgradeDeps, ip net.IP, targetImage string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(upgradePollInterval)
	defer ticker.Stop()

	var last string
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("node %s did not report the target version of %s within %s (last error: %w)",
					ip, targetImage, timeout, lastErr)
			}
			return fmt.Errorf("node %s did not report the target version of %s within %s (last reported %q)",
				ip, targetImage, timeout, last)
		case <-ticker.C:
		}

		version, err := deps.NodeVersion(ctx, ip)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		last = version
		if versionMatchesTarget(version, targetImage) {
			return nil
		}
	}
}

// buildUpgradePlan returns the ordered list of nodes to upgrade: control planes
// first (sorted by VMID), then workers (sorted by VMID). If nodeFilter is non-nil,
// only nodes whose IP appears in the filter are included.
func buildUpgradePlan(deployed *types.ClusterState, nodeFilter []net.IP) []upgradeNodeEntry {
	filterSet := make(map[string]bool, len(nodeFilter))
	for _, ip := range nodeFilter {
		filterSet[ip.String()] = true
	}
	include := func(ip net.IP) bool {
		if len(filterSet) == 0 {
			return true
		}
		return ip != nil && filterSet[ip.String()]
	}

	var plan []upgradeNodeEntry

	cps := make([]upgradeNodeEntry, 0, len(deployed.ControlPlanes))
	for _, cp := range deployed.ControlPlanes {
		if include(cp.IP) {
			cps = append(cps, upgradeNodeEntry{VMID: cp.VMID, IP: cp.IP, Role: types.RoleControlPlane})
		}
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].VMID < cps[j].VMID })
	plan = append(plan, cps...)

	workers := make([]upgradeNodeEntry, 0, len(deployed.Workers))
	for _, w := range deployed.Workers {
		if include(w.IP) {
			workers = append(workers, upgradeNodeEntry{VMID: w.VMID, IP: w.IP, Role: types.RoleWorker})
		}
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].VMID < workers[j].VMID })
	plan = append(plan, workers...)

	return plan
}

// parseNodeIPs parses a comma-separated list of IP addresses from a flag value.
// Returns nil when the input is empty (meaning "all nodes").
func parseNodeIPs(raw string) ([]net.IP, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ips := make([]net.IP, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		ip := net.ParseIP(p)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address in --nodes flag: %q", p)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// versionMatchesTarget returns true when the node's reported Talos version tag
// matches the tag embedded in the target installer image reference.
// e.g. nodeVersion="v1.13.4", targetImage="factory.talos.dev/installer:v1.13.4" → true
func versionMatchesTarget(nodeVersion, targetImage string) bool {
	if nodeVersion == "" || targetImage == "" {
		return false
	}
	// Strip any digest, then take the tag after the last path segment's colon so
	// a registry port ("host:5000/installer:v1.13.4") is not read as the tag.
	imageRef := strings.SplitN(targetImage, "@", 2)[0]
	lastSegment := imageRef[strings.LastIndex(imageRef, "/")+1:]
	i := strings.LastIndex(lastSegment, ":")
	if i < 0 || i == len(lastSegment)-1 {
		return false
	}
	return lastSegment[i+1:] == nodeVersion
}

// printUpgradePlan logs or displays the per-node action list for plan/dry-run mode.
func (app *App) printUpgradePlan(plan []upgradeNodeEntry, targetImage string, deps upgradeDeps, ctx context.Context) {
	app.Logger.Info("upgrade plan",
		zap.String("target_image", targetImage),
		zap.Int("total_nodes", len(plan)))

	for _, entry := range plan {
		current, err := deps.NodeVersion(ctx, entry.IP)
		if err != nil || current == "" {
			current = "<unreachable>"
		}
		skip := versionMatchesTarget(current, targetImage)
		action := "upgrade"
		if skip {
			action = "skip (already at target)"
		}
		app.Logger.Info("node",
			zap.Int("vmid", int(entry.VMID)),
			zap.String("ip", entry.IP.String()),
			zap.String("role", string(entry.Role)),
			zap.String("current_version", current),
			zap.String("action", action))
	}
}
