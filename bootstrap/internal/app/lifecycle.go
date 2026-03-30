package app

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/kubectl"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/talos"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

func (app *App) RunUp(ctx context.Context, skipInfra bool) error {
	if !skipInfra {
		// Gracefully remove nodes from the cluster BEFORE terraform destroys the VMs.
		// This ensures nodes are drained, removed from etcd, and deleted from K8s
		// while they're still reachable.
		if err := app.RunPreInfraRemovals(ctx); err != nil {
			app.Logger.Warn("pre-infra removal failed; continuing with infrastructure deploy",
				zap.String("hint", "nodes may not have been gracefully drained"),
				zap.Error(err))
		}

		tfDir, err := app.ResolveTerraformDir()
		if err != nil {
			return err
		}
		if err := app.RunInfraDeploy(ctx, tfDir, false); err != nil {
			return fmt.Errorf("up: infrastructure deploy failed: %w", err)
		}
	}

	// Full reconcile handles additions, bootstrap, config updates.
	// Removals will be no-ops since RunPreInfraRemovals already cleaned them from state.
	if err := app.RunReconcile(ctx); err != nil {
		app.Logger.Error("up: bootstrap failed; infrastructure is up",
			zap.String("hint", "run 'talops reconcile' to retry"))
		return fmt.Errorf("up: cluster bootstrap failed: %w", err)
	}
	return nil
}

// RunPreInfraRemovals detects nodes pending removal (present in deployed state but absent
// from desired state) and gracefully removes them from the cluster before terraform
// destroys the underlying VMs. This ensures proper drain, etcd member removal, and K8s
// node deletion while the nodes are still reachable.
func (app *App) RunPreInfraRemovals(ctx context.Context) error {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		return fmt.Errorf("resolve tfvars: %w", err)
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		return fmt.Errorf("load terraform extras: %w", err)
	}

	desired, err := stateMgr.LoadDesiredState(ctx)
	if err != nil {
		return fmt.Errorf("load desired state: %w", err)
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		return fmt.Errorf("load deployed state: %w", err)
	}

	if !deployed.BootstrapCompleted {
		return nil
	}

	// Build plan with nil live map - we only need the removal diff
	plan, err := stateMgr.BuildReconcilePlan(ctx, desired, deployed, nil)
	if err != nil {
		return fmt.Errorf("build removal plan: %w", err)
	}

	if len(plan.RemoveWorkers) == 0 && len(plan.RemoveControlPlanes) == 0 {
		return nil
	}

	app.Logger.Info("pre-infra removal: nodes need graceful removal before terraform destroy",
		zap.Int("workers", len(plan.RemoveWorkers)),
		zap.Int("control_planes", len(plan.RemoveControlPlanes)))

	audit := func(tag, msg string) {
		if app.Session != nil && app.Session.AuditLog != nil {
			app.Session.AuditLog.WriteEntry(tag, msg)
		}
	}
	audit("PRE-INFRA-REMOVAL-START", fmt.Sprintf("remove_workers=%d remove_cps=%d", len(plan.RemoveWorkers), len(plan.RemoveControlPlanes)))

	// Initialize clients needed for removal
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

	// Validate kubeconfig before kubectl calls
	if len(deployed.ControlPlanes) > 0 {
		kubeconfigMgr := talos.NewKubeconfigManager(talosClient, app.Logger)
		if err := kubeconfigMgr.Verify(ctx, cfg.ClusterName); err != nil {
			app.Logger.Warn("kubeconfig invalid, refreshing before removal",
				zap.Error(err))
			cpIP := deployed.ControlPlanes[0].IP
			if fetchErr := kubeconfigMgr.FetchAndMerge(ctx, cpIP, cfg.ClusterName, cfg.ControlPlaneEndpoint); fetchErr != nil {
				return fmt.Errorf("kubeconfig refresh for pre-infra removal: %w", fetchErr)
			}
		}
	}

	// Remove workers
	for _, vmid := range plan.RemoveWorkers {
		if cfg.DryRun {
			app.Logger.Info("would remove worker (pre-infra)", zap.Int("vmid", int(vmid)))
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

	// Remove control planes (with quorum check)
	if len(plan.RemoveControlPlanes) > 0 && len(deployed.ControlPlanes) > 0 && !cfg.DryRun {
		firstHealthyCP := deployed.ControlPlanes[0].IP
		remainingCPs := len(deployed.ControlPlanes)

		for i := range plan.RemoveControlPlanes {
			if err := talosClient.ValidateRemovalQuorum(ctx, firstHealthyCP, remainingCPs); err != nil {
				app.Logger.Error("quorum safety check failed", zap.Int("removal", i+1), zap.Error(err))
				return fmt.Errorf("quorum safety check failed for removal %d/%d: %w", i+1, len(plan.RemoveControlPlanes), err)
			}
			remainingCPs--
		}
	}

	for _, vmid := range plan.RemoveControlPlanes {
		if cfg.DryRun {
			app.Logger.Info("would remove control plane (pre-infra)", zap.Int("vmid", int(vmid)))
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
			// Find a healthy CP that isn't the one being removed for etcd operations
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

	// Persist state so RunReconcile sees the removals already done
	if !cfg.DryRun {
		deployed.Timestamp = time.Now()
		if err := stateMgr.Save(ctx, deployed); err != nil {
			return fmt.Errorf("save state after pre-infra removals: %w", err)
		}
	}

	audit("PRE-INFRA-REMOVAL-END", "status=success")
	app.Logger.Info("pre-infra removal complete")
	return nil
}

// removeNodeFromCluster handles the graceful removal of a single node: drain, etcd member
// removal (for CPs), K8s node deletion, and Talos reset. Errors are logged as warnings
// since the VM will be destroyed by terraform regardless.
func (app *App) removeNodeFromCluster(
	ctx context.Context,
	k8sClient *kubectl.Client,
	talosClient *talos.Client,
	vmid types.VMID,
	nodeIP net.IP,
	role types.Role,
	healthyEndpoint net.IP, // non-nil for CPs: another CP to query for etcd ops
) {
	nodeName, err := k8sClient.GetNodeNameByIP(ctx, nodeIP)
	if err != nil {
		app.Logger.Warn("failed to get node name", zap.Int("vmid", int(vmid)), zap.Error(err))
	} else {
		if err := k8sClient.DrainNode(ctx, nodeName); err != nil {
			app.Logger.Warn("failed to drain node", zap.String("node", nodeName), zap.Error(err))
		}
	}

	// Remove from etcd before deleting from K8s (control planes only)
	if role == types.RoleControlPlane && healthyEndpoint != nil {
		memberID, err := talosClient.GetEtcdMemberIDByIP(ctx, healthyEndpoint, nodeIP)
		if err != nil {
			app.Logger.Warn("failed to get etcd member ID", zap.Int("vmid", int(vmid)), zap.Error(err))
		} else {
			if err := talosClient.RemoveEtcdMember(ctx, healthyEndpoint, memberID); err != nil {
				app.Logger.Warn("failed to remove etcd member", zap.Int("vmid", int(vmid)), zap.Error(err))
			}
		}
	}

	if nodeName != "" {
		if err := k8sClient.DeleteNode(ctx, nodeName); err != nil {
			app.Logger.Warn("failed to delete node from k8s", zap.String("node", nodeName), zap.Error(err))
		}
	}

	if err := talosClient.ResetNode(ctx, nodeIP, true); err != nil {
		app.Logger.Warn("graceful reset failed, trying forced", zap.Int("vmid", int(vmid)), zap.Error(err))
		if err := talosClient.ResetNode(ctx, nodeIP, false); err != nil {
			app.Logger.Warn("forced reset also failed", zap.Int("vmid", int(vmid)), zap.Error(err))
		}
	}
}

// RunPruneNodes deletes NotReady K8s node objects that are not in the desired
// state. This is a standalone command for cleaning up ghost nodes left behind
// by previous scaling test runs or interrupted operations.
func (app *App) RunPruneNodes(ctx context.Context) error {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)

	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		return fmt.Errorf("resolve tfvars: %w", err)
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		return fmt.Errorf("load terraform extras: %w", err)
	}

	desired, err := stateMgr.LoadDesiredState(ctx)
	if err != nil {
		return fmt.Errorf("load deployed state: %w", err)
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		return fmt.Errorf("load deployed state: %w", err)
	}

	if !deployed.BootstrapCompleted {
		app.Logger.Info("Cluster not bootstrapped, nothing to prune")
		return nil
	}

	k8sClient := kubectl.NewClient(app.Logger)
	k8sClient.SetContext(cfg.ClusterName)
	if app.Session != nil && app.Session.AuditLog != nil {
		k8sClient.SetAuditLogger(app.Session.AuditLog)
	}

	// Validate kubeconfig before kubectl calls
	if len(deployed.ControlPlanes) > 0 {
		talosClient := talos.NewClient(cfg)
		talosClient.SetLogger(app.Logger)
		if err := talosClient.Initialize(ctx); err != nil {
			return fmt.Errorf("initialize talos client: %w", err)
		}

		kubeconfigMgr := talos.NewKubeconfigManager(talosClient, app.Logger)
		if err := kubeconfigMgr.Verify(ctx, cfg.ClusterName); err != nil {
			app.Logger.Warn("kubeconfig invalid, refreshing before prune", zap.Error(err))
			cpIP := deployed.ControlPlanes[0].IP
			if fetchErr := kubeconfigMgr.FetchAndMerge(ctx, cpIP, cfg.ClusterName, cfg.ControlPlaneEndpoint); fetchErr != nil {
				return fmt.Errorf("kubeconfig refresh for prune: %w", fetchErr)
			}
		}
	}

	deleted, err := app.SweepStaleNodes(ctx, k8sClient, desired, deployed)
	if err != nil {
		app.Logger.Warn("prune completed with errors", zap.Error(err))
	}

	action := "deleted"
	if cfg.DryRun {
		action = "would delete"
	}
	app.Logger.Info("prune-nodes complete",
		zap.String("action", action),
		zap.Int("stale_nodes", deleted))

	return err
}

func (app *App) RunDown(ctx context.Context, skipDrain, force bool) error {
	if !app.Cfg.AutoApprove {
		_, _ = fmt.Fprint(app.Session.Console, "This will DESTROY the cluster. Type \"yes\": ")
		var resp string
		_, _ = fmt.Scanln(&resp)
		_, _ = fmt.Fprintln(app.Session.ConsoleFile, resp)
		if resp != "yes" {
			app.Logger.Info("cancelled by user")
			return nil
		}
	}

	if !skipDrain {
		if err := app.drainAllNodes(ctx); err != nil {
			app.Logger.Warn("drain failed; continuing with destroy", zap.Error(err))
		}
	}

	tfDir, err := app.ResolveTerraformDir()
	if err != nil {
		return err
	}
	if err := app.RunInfraDestroy(ctx, tfDir, force, true); err != nil {
		return fmt.Errorf("down: destroy failed: %w", err)
	}

	// Reset bootstrap state so the next `talops up` starts fresh.
	// Without this, stale state causes the reconciler to skip bootstrap
	// and fail trying to reach a K8s API that doesn't exist yet.
	app.resetBootstrapState(ctx)

	return nil
}

// resetBootstrapState clears the bootstrap after a full cluster teardown.
// Preserves RemovedNodes for audit trail but marks the cluster as not bootstrapped
// and clears all active node lists.
func (app *App) resetBootstrapState(ctx context.Context) {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Warn("could not resolve tfvars for state reset", zap.Error(err))
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Warn("could not load terraform extras for state reset", zap.Error(err))
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		app.Logger.Warn("could not load deployed state for reset", zap.Error(err))
	}

	// Move all active nodes to removed audit trail
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

	deployed.ControlPlanes = nil
	deployed.Workers = nil
	deployed.BootstrapCompleted = false
	deployed.FirstControlPlane = 0
	deployed.Timestamp = now

	if err := stateMgr.Save(ctx, deployed); err != nil {
		app.Logger.Warn("failed to reset bootstrap state", zap.Error(err))
	} else {
		app.Logger.Info("bootstrap state reset after cluster teardown")
	}
}

func (app *App) drainAllNodes(ctx context.Context) error {
	stateMgr := state.NewManager(app.Cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		return fmt.Errorf("resolve tfvars: %w", err)
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		return fmt.Errorf("load terraform extras: %w", err)
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		return fmt.Errorf("load deployed state: %w", err)
	}

	k8sClient := kubectl.NewClient(app.Logger)
	k8sClient.SetContext(app.Cfg.ClusterName)
	if app.Session != nil && app.Session.AuditLog != nil {
		k8sClient.SetAuditLogger(app.Session.AuditLog)
	}

	var failCount int

	// Drain workers first, then control planes
	allNodes := append(deployed.Workers, deployed.ControlPlanes...)
	for _, node := range allNodes {
		if node.IP == nil {
			continue
		}
		nodeName, err := k8sClient.GetNodeNameByIP(ctx, node.IP)
		if err != nil {
			app.Logger.Warn("could not resolve node name", zap.Int("vmid", int(node.VMID)), zap.Error(err))
			failCount++
			continue
		}
		app.Logger.Info("draining node", zap.String("node", nodeName), zap.Int("vmid", int(node.VMID)))
		if err := k8sClient.DrainNode(ctx, nodeName); err != nil {
			app.Logger.Warn("drain failed", zap.String("node", nodeName), zap.Error(err))
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d node(s) failed to drain", failCount)
	}
	return nil
}
