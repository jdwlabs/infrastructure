package app

import (
	"context"
	"fmt"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

func (app *App) RunStatus(ctx context.Context) error {
	stateMgr := state.NewManager(app.Cfg, app.Logger)

	// Resolve and load additional fields from terraform.tfvars
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Warn("could not locate terraform.tfvars", zap.Error(err))
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Warn("could not load terraform extras", zap.String("path", app.Cfg.TerraformTFVars), zap.Error(err))
	}

	desired, err := stateMgr.LoadDesiredState(ctx)
	if err != nil {
		app.Logger.Error("failed to load desired state", zap.Error(err))
		return err
	}

	deployed, err := stateMgr.LoadDeployedState(ctx)
	if err != nil {
		app.Logger.Error("failed to load deployed state", zap.Error(err))
		return err
	}

	box := logging.NewBox(app.Session.Console, app.Cfg.NoColor)
	box.Header(fmt.Sprintf("CLUSTER STATUS: %s", app.Cfg.ClusterName))

	box.Label("Desired State (Terraform)")
	box.Row("Control Planes", fmt.Sprintf("%d", CountByRole(desired, types.RoleControlPlane)))
	box.Row("Workers", fmt.Sprintf("%d", CountByRole(desired, types.RoleWorker)))

	box.Section("Deployed State")
	box.Row("Control Planes", fmt.Sprintf("%d", len(deployed.ControlPlanes)))
	for _, cp := range deployed.ControlPlanes {
		box.Item("•", fmt.Sprintf("VMID %d: %s", cp.VMID, cp.IP))
	}
	box.Row("Workers", fmt.Sprintf("%d", len(deployed.Workers)))
	for _, w := range deployed.Workers {
		box.Item("•", fmt.Sprintf("VMID %d: %s", w.VMID, w.IP))
	}
	box.Row("Bootstrap Completed", fmt.Sprintf("%v", deployed.BootstrapCompleted))

	if deployed.TerraformHash != "" {
		currentHash, err := stateMgr.ComputeTerraformHash()
		if err == nil {
			if currentHash == deployed.TerraformHash {
				box.Row("Terraform Hash", fmt.Sprintf("%s (unchanged)", deployed.TerraformHash))
			} else {
				box.Row("Terraform Hash", fmt.Sprintf("%s (CHANGED from %s)", currentHash, deployed.TerraformHash))
			}
		}
	}

	box.Footer()
	return nil
}

func CountByRole(specs map[types.VMID]*types.NodeSpec, role types.Role) int {
	count := 0
	for _, spec := range specs {
		if spec.Role == role {
			count++
		}
	}
	return count
}
