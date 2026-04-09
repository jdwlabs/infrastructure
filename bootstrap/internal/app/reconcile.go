package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/discovery"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/kubectl"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/talos"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (app *App) RunReconcile(ctx context.Context) error {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	// Resolve terraform.tfvars path (tries configured path, then parent directory)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Warn("could not locate terraform.tfvars", zap.Error(err))
	}

	// Load additional fields from terraform.tfvars (cluster_name, proxmox tokens)
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Warn("could not load terraform extras", zap.String("path", cfg.TerraformTFVars), zap.Error(err))
	}

	if err := cfg.Validate(); err != nil {
		app.Logger.Error("configuration incomplete", zap.Error(err))
		return fmt.Errorf("configuration incomplete: %w", err)
	}

	app.Logger.Info("starting reconciliation",
		zap.String("cluster", cfg.ClusterName),
		zap.Bool("dry_run", cfg.DryRun),
		zap.Bool("plan_mode", cfg.PlanMode),
	)

	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("RECONCILE-START", fmt.Sprintf("cluster=%s dry_run=%v plan_mode=%v", cfg.ClusterName, cfg.DryRun, cfg.PlanMode))
	}

	if cfg.InsecureSSH {
		app.Logger.Warn("SSH host key verification is disabled (--insecure-ssh)")
	}
	scanner := discovery.NewScanner(cfg.ProxmoxSSHUser, cfg.ProxmoxNodeIPs, cfg.InsecureSSH)
	defer scanner.Close()
	talosClient := talos.NewClient(cfg)
	talosClient.SetLogger(app.Logger)
	if app.Session != nil && app.Session.AuditLog != nil {
		talosClient.SetAuditLogger(app.Session.AuditLog)
	}
	k8sClient := kubectl.NewClient(app.Logger)
	k8sClient.SetContext(cfg.ClusterName)
	if app.Session != nil && app.Session.AuditLog != nil {
		k8sClient.SetAuditLogger(app.Session.AuditLog)
	}

	// Configure SSH authentication for scanner
	if cfg.ProxmoxSSHKeyPath != "" {
		if err := scanner.SetPrivateKey(cfg.ProxmoxSSHKeyPath); err != nil {
			app.Logger.Warn("failed to set SSH private key for scanner", zap.String("key_path", cfg.ProxmoxSSHKeyPath), zap.Error(err))
		}
	}

	// Preflight: verify SSH connectivity to HAProxy before doing any work
	if !cfg.SkipPreflight && !cfg.DryRun && cfg.HAProxyIP != nil {
		haproxyPreflight := app.createHAProxyClient(cfg)
		if haproxyPreflight != nil {
			app.Logger.Info("preflight: checking HAProxy SSH connectivity",
				zap.String("host", cfg.HAProxyIP.String()),
				zap.String("user", cfg.HAProxyLoginUser))
			if err := haproxyPreflight.CheckConnectivity(); err != nil {
				app.Logger.Error("preflight: HAProxy SSH connectivity check failed",
					zap.String("host", cfg.HAProxyIP.String()),
					zap.String("user", cfg.HAProxyLoginUser),
					zap.Error(err))
				return fmt.Errorf("HAProxy SSH preflight failed (fix SSH auth to %s@%s or use --skip-preflight): %w",
					cfg.HAProxyLoginUser, cfg.HAProxyIP, err)
			}
			app.Logger.Info("preflight: HAProxy SSH connectivity OK")
		}
	}

	// Refresh Proxmox node IP map from the cluster
	if !cfg.SkipPreflight {
		app.Logger.Info("refreshing proxmox node IPs")
		scanner.RefreshProxmoxNodes(ctx)
	}

	// Initialize Talos client
	if err := talosClient.Initialize(ctx); err != nil {
		app.Logger.Error("failed to initialize talos client", zap.Error(err))
		return fmt.Errorf("initialize talos client: %w", err)
	}

	// Phase 1: Load states
	app.Logger.Info("loading desired state from terraform")
	desired, err := stateMgr.LoadDesiredState(ctx)
	if err != nil {
		app.Logger.Error("failed to load desired state", zap.Error(err))
		return fmt.Errorf("load desired state: %w", err)
	}
	if len(desired) == 0 {
		app.Logger.Error("no nodes defined in desired state")
		return fmt.Errorf("no nodes defined in desired state - check your terraform.tfvars")
	}
	app.Logger.Info("loaded desired state", zap.Int("nodes", len(desired)))

	// Generate node configs for any desired nodes missing configs
	for vmid, spec := range desired {
		configPath := stateMgr.NodeConfigPath(vmid, spec.Role)
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			app.Logger.Info("generating config for node", zap.Int("vmid", int(vmid)), zap.String("role", string(spec.Role)))
			if _, err := talosClient.GenerateNodeConfig(ctx, spec, cfg.SecretsDir); err != nil {
				app.Logger.Error("failed to generate node config", zap.Error(err))
				return fmt.Errorf("generate config for VMID %d: %w", vmid, err)
			}
		}
	}

	app.Logger.Info("loading deployed state")
	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		app.Logger.Error("failed to load deployed state", zap.Error(err))
		return fmt.Errorf("load deployed state: %w", err)
	}

	// Phase 2: Discovery
	app.Logger.Info("discovering live state")
	vmids := make([]types.VMID, 0, len(desired))
	for vmid := range desired {
		vmids = append(vmids, vmid)
	}

	var live map[types.VMID]*types.LiveNode
	if !cfg.SkipPreflight {
		if err := scanner.RepopulateARP(ctx); err != nil {
			app.Logger.Warn("ARP repopulation failed", zap.Error(err))
		}
		live, err = scanner.DiscoverVMs(ctx, vmids)
		if err != nil {
			app.Logger.Error("failed to discover VMs", zap.Error(err))
			return fmt.Errorf("discover VMs: %w", err)
		}
		app.Logger.Info("discovered live state", zap.Int("found", len(live)))

		for vmid, node := range live {
			if node.IP != nil {
				app.Logger.Debug("discovered VM", zap.Int("vmid", int(vmid)), zap.String("ip", node.IP.String()), zap.String("status", string(node.Status)))
			} else {
				app.Logger.Debug("discovered VM (no IP)", zap.Int("vmid", int(vmid)), zap.String("mac", node.MAC), zap.String("status", string(node.Status)))
			}
		}

		// Mark nodes that are already joined Talos cluster members
		if deployed.BootstrapCompleted && len(deployed.ControlPlanes) > 0 {
			if members, err := talosClient.GetClusterMembers(ctx, deployed.ControlPlanes[0].IP); err == nil {
				scanner.MarkJoinedNodes(members, live)
			}
		}

		// Preflight: verify Talos API (port 50000) is reachable on discovered VMs
		app.Logger.Info("running Talos API preflight checks")
		for vmid, node := range live {
			if node.IP == nil {
				continue
			}
			addr := fmt.Sprintf("%s:50000", node.IP)
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				app.Logger.Warn("Talos API not reachable on VM (may not be booted yet)",
					zap.Int("vmid", int(vmid)), zap.String("ip", node.IP.String()), zap.Error(err))
			} else {
				_ = conn.Close()
				app.Logger.Debug("Talos API reachable", zap.Int("vmid", int(vmid)), zap.String("ip", node.IP.String()))
			}
		}
	} else {
		live = make(map[types.VMID]*types.LiveNode)
	}

	// Phase 3: Build plan
	app.Logger.Info("building reconciliation plan")
	plan, err := stateMgr.BuildReconcilePlan(ctx, desired, deployed, live)
	if err != nil {
		app.Logger.Error("failed to build reconciliation plan", zap.Error(err))
		return fmt.Errorf("build plan: %w", err)
	}

	app.DisplayPlan(plan)

	if cfg.PlanMode {
		app.Logger.Info("plan mode - exiting without changes")
		return nil
	}

	if plan.IsEmpty() {
		app.Logger.Info("no changes required")
		return nil
	}

	// Confirm if not auto-approved
	if !cfg.AutoApprove && !cfg.DryRun {
		if !app.PromptConfirm("Proceed with changes? [y/N]: ") {
			return nil
		}
	}

	// Phase 4: Execute
	if err := app.executePlan(ctx, plan, desired, deployed, live, stateMgr, scanner, talosClient, k8sClient); err != nil {
		app.Logger.Error("plan execution failed", zap.Error(err))
		if app.Session != nil && app.Session.AuditLog != nil {
			app.Session.AuditLog.WriteEntry("RECONCILE-END", fmt.Sprintf("cluster=%s status=failed error=%v", cfg.ClusterName, err))
		}
		return fmt.Errorf("execute plan: %w", err)
	}

	app.Logger.Info("reconciliation complete")
	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("RECONCILE-END", fmt.Sprintf("cluster=%s status=success", cfg.ClusterName))
	}
	return nil
}

// updateSessionCounters refreshes the session counters from the current plan and deployed state.
// Called at the start of executePlan and again after bootstrap recovery rebuilds the plan.
func (app *App) updateSessionCounters(plan *types.ReconcilePlan, deployed *types.ClusterState) {
	if app.Session == nil {
		return
	}
	app.Session.ControlPlanes = len(deployed.ControlPlanes)
	app.Session.Workers = len(deployed.Workers)
	app.Session.AddedNodes = len(plan.AddControlPlanes) + len(plan.AddWorkers)
	app.Session.RemovedNodes = len(plan.RemoveControlPlanes) + len(plan.RemoveWorkers)
	app.Session.UpdatedConfigs = len(plan.UpdateConfigs)
	app.Session.BootstrapNeeded = plan.NeedsBootstrap
}

// executeBootstrap deploys the first control plane (if needed) and bootstraps etcd.
// Handle two cases:
//  1. Fresh bootstrap: deploy first CP from AddControlPlanes, then bootstrap etcd
//  2. Deferred bootstrap: CPs already deployed but etcd never bootstrapped (e.g. previous
//     run applied configs but crashed before etcd init) - bootstrap etcd on existing CP
//
// Returns the VMID of the bootstrapped node so Phase 3 can skip re-deploying it.
func (app *App) executeBootstrap(
	ctx context.Context,
	plan *types.ReconcilePlan,
	desired map[types.VMID]*types.NodeSpec,
	deployed *types.ClusterState,
	live map[types.VMID]*types.LiveNode,
	stateMgr *state.Manager,
	scanner *discovery.Scanner,
	talosClient *talos.Client,
	audit func(string, string),
) (types.VMID, error) {
	cfg := app.Cfg

	app.Logger.Info("executing bootstrap")
	audit("BOOTSTRAP-START", fmt.Sprintf("control_planes=%d workers=%d", len(plan.AddControlPlanes), len(plan.AddWorkers)))

	if len(plan.AddControlPlanes) > 0 {
		// Fresh bootstrap: deploy the first CP then bootstrap etcd on it
		firstVMID := plan.AddControlPlanes[0]
		spec := desired[firstVMID]

		if cfg.DryRun {
			app.Logger.Info("would bootstrap first control plane",
				zap.Int("vmid", int(firstVMID)),
				zap.String("name", spec.Name))
			deployed.BootstrapCompleted = true
			if err := stateMgr.Save(ctx, deployed); err != nil {
				app.Logger.Error("failed to save state after bootstrap", zap.Error(err))
				return firstVMID, fmt.Errorf("save state after bootstrap: %w", err)
			}
			return firstVMID, nil
		}

		newIP, err := app.deployNode(ctx, firstVMID, types.RoleControlPlane, live, deployed, stateMgr, scanner, talosClient)
		if err != nil {
			app.Logger.Error("bootstrap first control plane failed", zap.Int("vmid", int(firstVMID)), zap.Error(err))
			return 0, fmt.Errorf("bootstrap first CP: %w", err)
		}

		if err := app.bootstrapEtcdAndWait(ctx, talosClient, newIP, firstVMID); err != nil {
			return 0, err
		}

		deployed.BootstrapCompleted = true
		if err := stateMgr.Save(ctx, deployed); err != nil {
			app.Logger.Error("failed to save state after bootstrap", zap.Error(err))
			return firstVMID, fmt.Errorf("save state after bootstrap: %w", err)
		}

		// Wait for other CPs to join etcd (they will be deployed in Phase 3).
		// For a fresh bootstrap only the first CP is deployed here; the rest join later.
		// No quorum wait needed yet - Phase 3 handles the remaining CPs.
		return firstVMID, nil
	}

	// Deferred bootstrap: CPs already in deployed state but etcd was never initialized.
	// This happens when a previous run deployed Talos configs but failed before etcd bootstrap.
	if len(deployed.ControlPlanes) > 0 {
		firstCP := deployed.ControlPlanes[0]

		if cfg.DryRun {
			app.Logger.Info("would bootstrap etcd on existing first control plane",
				zap.Int("vmid", int(firstCP.VMID)))
			deployed.BootstrapCompleted = true
			if err := stateMgr.Save(ctx, deployed); err != nil {
				return firstCP.VMID, fmt.Errorf("save state after deferred bootstrap: %w", err)
			}
			return firstCP.VMID, nil
		}
		app.Logger.Info("bootstrapping etcd on already-deployed control plane",
			zap.String("ip", firstCP.IP.String()), zap.Int("vmid", int(firstCP.VMID)))

		if err := app.bootstrapEtcdAndWait(ctx, talosClient, firstCP.IP, firstCP.VMID); err != nil {
			return 0, err
		}

		deployed.BootstrapCompleted = true
		if err := stateMgr.Save(ctx, deployed); err != nil {
			return firstCP.VMID, fmt.Errorf("save state after deferred bootstrap: %w", err)
		}

		// In deferred mode, all CPs are already deployed with configs.
		// Wait for them to join etcd so we have quorum before K8s API starts.
		if len(deployed.ControlPlanes) > 1 {
			if err := talosClient.WaitForEtcdMembers(ctx, firstCP.IP, len(deployed.ControlPlanes), 3*time.Minute); err != nil {
				app.Logger.Warn("not all CPs joined etcd within timeout; continuing",
					zap.Int("expected", len(deployed.ControlPlanes)),
					zap.Error(err))
			}
		}

		return firstCP.VMID, nil
	}

	app.Logger.Warn("bootstrap requested but no control planes available")
	return 0, nil
}

// bootstrapEtcdAndWait runs etcd bootstrap on a control plane and waits for it to be healthy.
func (app *App) bootstrapEtcdAndWait(ctx context.Context, talosClient *talos.Client, ip net.IP, vmid types.VMID) error {
	app.Logger.Info("bootstrapping etcd on first control plane", zap.String("ip", ip.String()), zap.Int("vmid", int(vmid)))
	if err := talosClient.BootstrapEtcd(ctx, ip); err != nil {
		app.Logger.Error("etcd bootstrap failed", zap.String("ip", ip.String()), zap.Int("vmid", int(vmid)), zap.Error(err))
		return fmt.Errorf("bootstrap etcd: %w", err)
	}

	if err := talosClient.WaitForEtcdHealthy(ctx, ip, 5*time.Minute); err != nil {
		app.Logger.Error("etcd health check timed out", zap.String("ip", ip.String()), zap.Int("vmid", int(vmid)), zap.Error(err))
		return fmt.Errorf("wait for etcd healthy: %w", err)
	}
	app.Logger.Info("first control plane ready (API + etcd)", zap.String("ip", ip.String()), zap.Int("vmid", int(vmid)))
	return nil
}

// deployNode handles the apply -> reboot -> wait -> hash flow for adding a node
func (app *App) deployNode(
	ctx context.Context,
	vmid types.VMID,
	role types.Role,
	live map[types.VMID]*types.LiveNode,
	deployed *types.ClusterState,
	stateMgr *state.Manager,
	scanner *discovery.Scanner,
	talosClient *talos.Client,
) (net.IP, error) {
	node, ok := live[vmid]
	if !ok || node.IP == nil {
		// Retry IP discovery up to 3 times with 10s intervals - VMs may still be booting
		const maxRetries = 3
		for attempt := 1; attempt <= maxRetries; attempt++ {
			app.Logger.Info("VM not in live map, re-discovering",
				zap.Int("vmid", int(vmid)),
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxRetries))
			if err := scanner.RepopulateARP(ctx); err != nil {
				app.Logger.Warn("ARP repopulation failed", zap.Error(err))
			}
			liveNodes, err := scanner.DiscoverVMs(ctx, []types.VMID{vmid})
			if err != nil {
				app.Logger.Error("failed to discover VM", zap.Int("vmid", int(vmid)), zap.Error(err))
			} else {
				node, ok = liveNodes[vmid]
				if ok && node.IP != nil {
					break
				}
			}
			if attempt < maxRetries {
				app.Logger.Info("VM IP not yet available, waiting before retry",
					zap.Int("vmid", int(vmid)),
					zap.Duration("wait", 10*time.Second))
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("context cancelled waiting for VM %d IP: %w", vmid, ctx.Err())
				case <-time.After(10 * time.Second):
				}
			}
		}
		if !ok || node == nil || node.IP == nil {
			app.Logger.Error("VM IP not discovered after retries", zap.Int("vmid", int(vmid)))
			return nil, fmt.Errorf("VM %d IP not discovered after %d attempts", vmid, maxRetries)
		}
	}

	configPath := stateMgr.NodeConfigPath(vmid, role)
	app.Logger.Info("applying config", zap.Int("vmid", int(vmid)), zap.String("role", string(role)))
	if app.Session != nil && app.Session.AuditLog != nil {
		app.Session.AuditLog.WriteEntry("APPLY-CONFIG", fmt.Sprintf("vmid=%d role=%s ip=%s", vmid, role, node.IP))
	}
	if err := talosClient.ApplyConfigWithRetry(ctx, node.IP, configPath, role, 5); err != nil {
		app.Logger.Error("failed to apply config", zap.Int("vmid", int(vmid)), zap.String("role", string(role)), zap.Error(err))
		return nil, fmt.Errorf("apply config to %s %d: %w", role, vmid, err)
	}

	monitor := discovery.NewRebootMonitor(vmid, node.IP, node.MAC, scanner, app.Logger)
	newIP, err := monitor.WaitForReady(ctx, 5*time.Minute)
	if err != nil {
		app.Logger.Error("node reboot wait failed", zap.Int("vmid", int(vmid)), zap.String("role", string(role)), zap.Error(err))
		return nil, fmt.Errorf("wait for %s %d reboot: %w", role, vmid, err)
	}

	if err := talosClient.WaitForAPI(ctx, newIP); err != nil {
		app.Logger.Error("Talos API not reachable", zap.Int("vmid", int(vmid)), zap.String("role", string(role)), zap.Error(err))
		return nil, fmt.Errorf("wait for %s %d API: %w", role, vmid, err)
	}

	configHash, hashErr := talos.HashFile(configPath)
	if hashErr != nil {
		app.Logger.Warn("failed to hash config file", zap.Int("vmid", int(vmid)), zap.Error(hashErr))
	}
	stateMgr.UpdateNodeState(deployed, vmid, newIP.String(), configHash, role)

	app.Logger.Info("node deployed, Talos API responding", zap.Int("vmid", int(vmid)), zap.String("role", string(role)), zap.String("ip", newIP.String()))
	return newIP, nil
}

func (app *App) executePlan(
	ctx context.Context,
	plan *types.ReconcilePlan,
	desired map[types.VMID]*types.NodeSpec,
	deployed *types.ClusterState,
	live map[types.VMID]*types.LiveNode,
	stateMgr *state.Manager,
	scanner *discovery.Scanner,
	talosClient *talos.Client,
	k8sClient *kubectl.Client,
) error {
	cfg := app.Cfg

	// Populate session counters early so SUMMARY.txt has data even on error
	app.updateSessionCounters(plan, deployed)

	var bootstrappedVMID types.VMID

	audit := func(tag, msg string) {
		if app.Session != nil && app.Session.AuditLog != nil {
			app.Session.AuditLog.WriteEntry(tag, msg)
		}
	}

	// Phase 0: Bootstrap first CP if needed (or deferred etcd bootstrap on existing CP)
	if plan.NeedsBootstrap {
		vmid, err := app.executeBootstrap(ctx, plan, desired, deployed, live, stateMgr, scanner, talosClient, audit)
		if err != nil {
			return err
		}
		bootstrappedVMID = vmid
	}

	// Validate kubeconfig before any kubectl operations.
	// During reconciliation (not initial bootstrap), the kubeconfig may contain a stale
	// CA certificate if cluster secrets were regenerated. Re-fetch from Talos API if invalid.
	if deployed.BootstrapCompleted && !plan.NeedsBootstrap && !cfg.DryRun && len(deployed.ControlPlanes) > 0 {
		kubeconfigMgr := talos.NewKubeconfigManager(talosClient, app.Logger)
		if err := kubeconfigMgr.Verify(ctx, cfg.ClusterName); err != nil {
			app.Logger.Warn("kubeconfig validation failed, refreshing from Talos API",
				zap.String("cluster", cfg.ClusterName),
				zap.Error(err))
			audit("KUBECONFIG-REFRESH", fmt.Sprintf("reason=validation_failed cluster=%s", cfg.ClusterName))

			cpIP := deployed.ControlPlanes[0].IP
			if fetchErr := kubeconfigMgr.FetchAndMerge(ctx, cpIP, cfg.ClusterName, cfg.ControlPlaneEndpoint); fetchErr != nil {
				// K8s API unreachable on the CP - this likely means the VMs were
				// recreated (e.g. after `talops down`) and need a bootstrap.
				// Reset state and convert the plan to a fresh bootstrap instead of aborting.
				app.Logger.Warn("K8s API unreachable on control plane, resetting to bootstrap mode",
					zap.String("ip", cpIP.String()),
					zap.Error(fetchErr))

				// Preserve audit trail before clearing state
				now := time.Now()
				for _, cp := range deployed.ControlPlanes {
					cp.Role = types.RoleControlPlane
					cp.RemovedAt = &now
					deployed.RemovedNodes = append(deployed.RemovedNodes, cp)
				}
				for _, w := range deployed.Workers {
					w.Role = types.RoleWorker
					w.RemovedAt = &now
					deployed.RemovedNodes = append(deployed.RemovedNodes, w)
				}

				deployed.BootstrapCompleted = false
				deployed.ControlPlanes = nil
				deployed.Workers = nil
				deployed.FirstControlPlane = 0

				// Rebuild the plan as a fresh bootstrap
				newPlan, rebuildErr := stateMgr.BuildReconcilePlan(ctx, desired, deployed, live)
				if rebuildErr != nil {
					return fmt.Errorf("rebuild plan for bootstrap recovery: %w", rebuildErr)
				}
				newPlan.NeedsBootstrap = true
				plan = newPlan
				app.Logger.Info("recovered to bootstrap mode",
					zap.Int("add_cps", len(plan.AddControlPlanes)),
					zap.Int("add_workers", len(plan.AddWorkers)))
				app.DisplayPlan(plan)

				// Update session counters to reflect the recovery plan
				app.updateSessionCounters(plan, deployed)

				// Execute bootstrap now - Phase 0 already passed, so we must run it here
				vmid, err := app.executeBootstrap(ctx, plan, desired, deployed, live, stateMgr, scanner, talosClient, audit)
				if err != nil {
					return err
				}
				bootstrappedVMID = vmid
			} else {
				if verifyErr := kubeconfigMgr.Verify(ctx, cfg.ClusterName); verifyErr != nil {
					app.Logger.Error("kubeconfig still invalid after refresh", zap.Error(verifyErr))
					return fmt.Errorf("kubeconfig verification after refresh: %w", verifyErr)
				}
				app.Logger.Info("kubeconfig refreshed successfully", zap.String("cluster", cfg.ClusterName))
			}
		}
	}

	// Phase 1: Remove workers
	if len(plan.RemoveWorkers) > 0 {
		app.Logger.Info("removing workers", zap.Int("count", len(plan.RemoveWorkers)))
		audit("REMOVE-WORKERS", fmt.Sprintf("count=%d vmids=%v", len(plan.RemoveWorkers), plan.RemoveWorkers))

		for _, vmid := range plan.RemoveWorkers {
			if cfg.DryRun {
				app.Logger.Info("would remove worker", zap.Int("vmid", int(vmid)))
				continue
			}

			var nodeIP net.IP
			for _, w := range deployed.Workers {
				if w.VMID == vmid {
					nodeIP = w.IP
					break
				}
			}

			if nodeIP != nil {
				app.removeNodeFromCluster(ctx, k8sClient, talosClient, vmid, nodeIP, types.RoleWorker, nil)
			}

			stateMgr.RemoveNodeState(deployed, vmid, types.RoleWorker)
		}
	}

	// Phase 2: Remove control planes (with quorum check)
	if len(plan.RemoveControlPlanes) > 0 {
		app.Logger.Info("removing control planes", zap.Int("count", len(plan.RemoveControlPlanes)))
		audit("REMOVE-CPS", fmt.Sprintf("count=%d vmids=%v", len(plan.RemoveControlPlanes), plan.RemoveControlPlanes))

		if len(deployed.ControlPlanes) > 0 && !cfg.DryRun {
			firstHealthyCP := deployed.ControlPlanes[0].IP
			remainingCPs := len(deployed.ControlPlanes)

			for i := range plan.RemoveControlPlanes {
				if err := talosClient.ValidateRemovalQuorum(ctx, firstHealthyCP, remainingCPs); err != nil {
					app.Logger.Error("quorum safety check failed", zap.Int("removal", i+1), zap.Int("total", len(plan.RemoveControlPlanes)), zap.Error(err))
					return fmt.Errorf("quorum safety check failed for removal %d/%d: %w", i+1, len(plan.RemoveControlPlanes), err)
				}
				remainingCPs--
			}

			app.Logger.Info("quorum safety check passed",
				zap.Int("current_cps", len(deployed.ControlPlanes)),
				zap.Int("removing", len(plan.RemoveControlPlanes)))
		}

		for _, vmid := range plan.RemoveControlPlanes {
			if cfg.DryRun {
				app.Logger.Info("would remove control plane", zap.Int("vmid", int(vmid)))
				continue
			}

			var nodeIP net.IP
			for _, cp := range deployed.ControlPlanes {
				if cp.VMID == vmid {
					nodeIP = cp.IP
					break
				}
			}

			if nodeIP != nil {
				var healthyEndpoint net.IP
				for _, cp := range deployed.ControlPlanes {
					if cp.VMID != vmid {
						healthyEndpoint = cp.IP
						break
					}
				}
				app.removeNodeFromCluster(ctx, k8sClient, talosClient, vmid, nodeIP, types.RoleControlPlane, healthyEndpoint)
			}

			stateMgr.RemoveNodeState(deployed, vmid, types.RoleControlPlane)
		}
	}

	// Phase 3: Add remaining control planes (sequential for etcd safety)
	if len(plan.AddControlPlanes) > 0 {
		app.Logger.Info("adding control planes", zap.Int("count", len(plan.AddControlPlanes)))
		audit("ADD-CPS", fmt.Sprintf("count=%d vmids=%v", len(plan.AddControlPlanes), plan.AddControlPlanes))

		for _, vmid := range plan.AddControlPlanes {
			if plan.NeedsBootstrap && vmid == bootstrappedVMID {
				continue
			}

			spec := desired[vmid]

			if cfg.DryRun {
				app.Logger.Info("would add control plane",
					zap.Int("vmid", int(vmid)),
					zap.String("name", spec.Name),
				)
				continue
			}

			if _, err := app.deployNode(ctx, vmid, types.RoleControlPlane, live, deployed, stateMgr, scanner, talosClient); err != nil {
				app.Logger.Error("failed to add control plane", zap.Int("vmid", int(vmid)), zap.Error(err))
				return fmt.Errorf("add CP %d: %w", vmid, err)
			}
		}
	}

	// Phase 4: Update HAProxy config
	// Always update when CPs exist - CP IPs may change during reboots (DHCP)
	// even when no CP membership change occurred.
	if cfg.DryRun && len(deployed.ControlPlanes) > 0 {
		app.Logger.Info("would update HAProxy configuration", zap.Int("backends", len(deployed.ControlPlanes)))
	} else if !cfg.DryRun && len(deployed.ControlPlanes) > 0 {
		haproxyConfig := haproxy.ConfigFromClusterState(cfg, deployed)
		configStr, err := haproxyConfig.Generate()
		if err != nil {
			app.Logger.Error("failed to generate HAProxy config", zap.Error(err))
			return fmt.Errorf("generate HAProxy config: %w", err)
		}

		haproxyClient := app.createHAProxyClient(cfg)
		if haproxyClient == nil {
			app.Logger.Error("HAProxy SSH auth not configured (no key file and no SSH agent)")
			return fmt.Errorf("HAProxy SSH auth not configured: set --ssh-key, --haproxy-ssh-key, or ensure SSH_AUTH_SOCK is available")
		}

		if err := haproxyClient.Update(ctx, configStr); err != nil {
			app.Logger.Error("HAProxy update failed",
				zap.String("host", cfg.HAProxyIP.String()),
				zap.String("user", cfg.HAProxyLoginUser),
				zap.Error(err))
			return fmt.Errorf("HAProxy update failed: %w", err)
		}
	}

	// Phase 5: Fetch kubeconfig after bootstrap
	if plan.NeedsBootstrap && deployed.BootstrapCompleted && !cfg.DryRun {
		app.EnsureEndpointResolvable()

		kubeconfigMgr := talos.NewKubeconfigManager(talosClient, app.Logger)
		if len(deployed.ControlPlanes) > 0 {
			cpIP := deployed.ControlPlanes[0].IP

			// Wait for K8s API to accept connections. After etcd bootstrap the API
			// server can take 1-3 minutes to start (needs etcd quorum, scheduler,
			// controller-manager initialization).
			if err := kubeconfigMgr.WaitForKubernetesAPI(ctx, cpIP, 5*time.Minute); err != nil {
				app.Logger.Warn("Kubernetes API did not become reachable within timeout",
					zap.String("ip", cpIP.String()),
					zap.Error(err))
				// Non-fatal: kubeconfig fetch will fail below but we continue
			}

			// Retry kubeconfig fetch - even after the API port opens, the first
			// attempt can race with TLS certificate provisioning.
			var kubeconfigFetched bool
			const maxFetchAttempts = 3
			for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
				if err := kubeconfigMgr.FetchAndMerge(ctx, cpIP, cfg.ClusterName, cfg.ControlPlaneEndpoint); err != nil {
					app.Logger.Warn("kubeconfig fetch attempt failed",
						zap.Int("attempt", attempt),
						zap.Int("max_attempts", maxFetchAttempts),
						zap.Error(err))
					if attempt < maxFetchAttempts {
						select {
						case <-ctx.Done():
							break
						case <-time.After(15 * time.Second):
						}
					}
					continue
				}
				kubeconfigFetched = true
				break
			}

			if kubeconfigFetched {
				if err := kubeconfigMgr.Verify(ctx, cfg.ClusterName); err != nil {
					app.Logger.Warn("kubeconfig verification failed after fetch", zap.Error(err))
				}
			} else {
				app.Logger.Warn("kubeconfig fetch failed after all attempts (can retry with 'talops reconcile')")
			}
		}

		// talosctl config is updated after Phase 8 for all reconcile runs
	}

	// Phase 6: Add workers (parallel, max 3 concurrent)
	if len(plan.AddWorkers) > 0 {
		app.Logger.Info("adding workers", zap.Int("count", len(plan.AddWorkers)))
		audit("ADD-WORKERS", fmt.Sprintf("count=%d vmids=%v", len(plan.AddWorkers), plan.AddWorkers))

		g, gctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, 3)

		for _, vmid := range plan.AddWorkers {
			vmid, spec := vmid, desired[vmid]

			g.Go(func() error {
				sem <- struct{}{}
				defer func() { <-sem }()

				select {
				case <-gctx.Done():
					return gctx.Err()
				default:
				}

				if cfg.DryRun {
					app.Logger.Info("would add worker",
						zap.Int("vmid", int(vmid)),
						zap.String("name", spec.Name))
					return nil
				}

				if _, err := app.deployNode(gctx, vmid, types.RoleWorker, live, deployed, stateMgr, scanner, talosClient); err != nil {
					return fmt.Errorf("add worker %d: %w", vmid, err)
				}
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			app.Logger.Error("worker deployment failed", zap.Error(err))
			return err
		}
	}

	// Phase 6b: Wait for newly added nodes to reach Ready
	newNodeVMIDs := append(plan.AddControlPlanes, plan.AddWorkers...)
	if len(newNodeVMIDs) > 0 && !cfg.DryRun && deployed.BootstrapCompleted {
		app.Logger.Info("waiting for new nodes to become Ready", zap.Int("count", len(newNodeVMIDs)))
		const nodeReadyTimeout = 5 * time.Minute

		for _, vmid := range newNodeVMIDs {
			// Skip the bootstrapped CP (already handled by etcd health wait)
			if plan.NeedsBootstrap && vmid == bootstrappedVMID {
				continue
			}

			var nodeIP net.IP
			for _, cp := range deployed.ControlPlanes {
				if cp.VMID == vmid {
					nodeIP = cp.IP
					break
				}
			}
			if nodeIP == nil {
				for _, w := range deployed.Workers {
					if w.VMID == vmid {
						nodeIP = w.IP
						break
					}
				}
			}
			if nodeIP == nil {
				app.Logger.Warn("cannot wait for node readiness: IP not in deployed state", zap.Int("vmid", int(vmid)))
				continue
			}

			nodeName, err := k8sClient.GetNodeNameByIP(ctx, nodeIP)
			if err != nil {
				app.Logger.Warn("cannot resolve node name for readiness wait", zap.Int("vmid", int(vmid)), zap.Error(err))
				continue
			}

			if err := k8sClient.WaitForNodeReady(ctx, nodeName, nodeReadyTimeout); err != nil {
				app.Logger.Warn("node readiness wait timed out", zap.Int("vmid", int(vmid)), zap.String("node", nodeName), zap.Error(err))
			} else {
				app.Logger.Info("node is Ready", zap.Int("vmid", int(vmid)), zap.String("node", nodeName))
			}
		}
	}

	// Phase 6c: Re-update HAProxy with workers included in ingress backends
	// Phase 4 runs before workers are deployed so IngressNodes only has CPs.
	// Now that workers are deployed and ready, re-generate to include them.
	if len(plan.AddWorkers) > 0 && !cfg.DryRun && len(deployed.ControlPlanes) > 0 {
		haproxyConfig := haproxy.ConfigFromClusterState(cfg, deployed)
		configStr, err := haproxyConfig.Generate()
		if err != nil {
			app.Logger.Error("failed to generate HAProxy config with workers", zap.Error(err))
			return fmt.Errorf("generate HAProxy config (post-worker): %w", err)
		}

		haproxyClient := app.createHAProxyClient(cfg)
		if haproxyClient == nil {
			app.Logger.Error("HAProxy SSH auth not configured (no key file and no SSH agent)")
			return fmt.Errorf("HAProxy SSH auth not configured: set --ssh-key, --haproxy-ssh-key, or ensure SSH_AUTH_SOCK is available")
		}

		if err := haproxyClient.Update(ctx, configStr); err != nil {
			app.Logger.Error("HAProxy update with workers failed",
				zap.String("host", cfg.HAProxyIP.String()),
				zap.String("user", cfg.HAProxyLoginUser),
				zap.Error(err))
			return fmt.Errorf("HAProxy update (post-worker) failed: %w", err)
		}
		app.Logger.Info("HAProxy updated with worker ingress nodes", zap.Int("ingressNodes", len(haproxyConfig.IngressNodes)))
	}

	// Phase 7: Update configs
	if len(plan.UpdateConfigs) > 0 {
		app.Logger.Info("updating configurations", zap.Int("count", len(plan.UpdateConfigs)))

		for _, vmid := range plan.UpdateConfigs {
			spec, exists := desired[vmid]
			if !exists {
				continue
			}

			if cfg.DryRun {
				app.Logger.Info("would update config", zap.Int("vmid", int(vmid)))
				continue
			}

			var nodeIP net.IP
			for _, cp := range deployed.ControlPlanes {
				if cp.VMID == vmid {
					nodeIP = cp.IP
					break
				}
			}
			if nodeIP == nil {
				for _, w := range deployed.Workers {
					if w.VMID == vmid {
						nodeIP = w.IP
						break
					}
				}
			}

			if nodeIP == nil {
				app.Logger.Warn("cannot update config - node IP not found", zap.Int("vmid", int(vmid)))
				continue
			}

			if _, err := talosClient.GenerateNodeConfig(ctx, spec, cfg.SecretsDir); err != nil {
				app.Logger.Warn("failed to regenerate config for node", zap.Int("vmid", int(vmid)), zap.Error(err))
				continue
			}

			configPath := stateMgr.NodeConfigPath(vmid, spec.Role)
			app.Logger.Info("applying updated config", zap.Int("vmid", int(vmid)))

			if err := talosClient.ApplyConfig(ctx, nodeIP, configPath, false); err != nil {
				app.Logger.Warn("failed to apply updated config", zap.Int("vmid", int(vmid)), zap.Error(err))
				continue
			}

			configHash, hashErr := talos.HashFile(configPath)
			if hashErr != nil {
				app.Logger.Warn("failed to hash config file", zap.Int("vmid", int(vmid)), zap.Error(hashErr))
			}
			stateMgr.UpdateNodeState(deployed, vmid, nodeIP.String(), configHash, spec.Role)
		}
	}

	// Phase 8: Save final state
	if !cfg.DryRun {
		deployed.Timestamp = time.Now()
		if err := stateMgr.Save(ctx, deployed); err != nil {
			app.Logger.Error("failed to save final state", zap.Error(err))
			return fmt.Errorf("save state: %w", err)
		}
	}

	// Phase 9: Configure talosctl endpoints/nodes and merge into ~/.talos/config
	if !cfg.DryRun && deployed.BootstrapCompleted && len(deployed.ControlPlanes) > 0 {
		app.ConfigureTalosctl(deployed)
	}

	// Phase 10: Post-reconciliation verification
	if !cfg.DryRun && deployed.BootstrapCompleted {
		app.VerifyCluster(ctx, talosClient, k8sClient, deployed)
	}

	// Phase 11: Sweep stale K8s node objects
	if deployed.BootstrapCompleted {
		deleted, err := app.SweepStaleNodes(ctx, k8sClient, desired, deployed)
		if err != nil {
			app.Logger.Warn("stale node sweep completed with errors", zap.Error(err))
		}
		if deleted > 0 {
			action := "deleted"
			if cfg.DryRun {
				action = "would delete"
			}
			app.Logger.Info("stale node sweep complete",
				zap.String("action", action),
				zap.Int("count", deleted))
			audit("SWEEP-STALE-NODES", fmt.Sprintf("action=%s count=%d", action, deleted))
		}
	}

	// Update session counters with final deployed state
	if app.Session != nil {
		app.Session.ControlPlanes = len(deployed.ControlPlanes)
		app.Session.Workers = len(deployed.Workers)
	}

	return nil
}

// createHAProxyClient builds an HAProxy SSH client with the best available auth method.
// Tries, in order: explicit haproxy key, proxmox key (with agent fallback), agent-only.
// Returns nil if no auth method is available.
func (app *App) createHAProxyClient(cfg *types.Config) *haproxy.Client {
	client := haproxy.NewClient(cfg.HAProxyLoginUser, cfg.HAProxyIP.String(), app.Logger, cfg.InsecureSSH)

	haproxyKeyPath := cfg.HAProxySSHKeyPath
	if haproxyKeyPath == "" {
		haproxyKeyPath = cfg.ProxmoxSSHKeyPath
	}

	if haproxyKeyPath != "" {
		if err := client.SetPrivateKey(haproxyKeyPath); err != nil {
			app.Logger.Warn("failed to load SSH key for HAProxy, trying SSH agent",
				zap.String("key_path", haproxyKeyPath),
				zap.Error(err))
			if !client.SetSSHAgent() {
				app.Logger.Error("no SSH auth available for HAProxy",
					zap.String("key_path", haproxyKeyPath),
					zap.String("host", cfg.HAProxyIP.String()))
				return nil
			}
		}
		return client
	}

	// No key path configured - try SSH agent only
	if client.SetSSHAgent() {
		return client
	}
	return nil
}
