package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/discovery"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/terraform"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

// ResolveTerraformDir resolves the terraform directory using heuristic search.
// Priority: --terraform-dir flag > TERRAFORM_DIR env > auto-detect.
func (app *App) ResolveTerraformDir() (string, error) {
	if app.Cfg.TerraformDir != "" {
		return app.Cfg.TerraformDir, nil
	}
	if v := os.Getenv("TERRAFORM_DIR"); v != "" {
		return v, nil
	}
	candidates := []string{".", "../terraform", "terraform", ".."}
	for _, dir := range candidates {
		if hasTerraformFiles(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("terraform directory not found; use --terraform-dir or TERRAFORM_DIR")
}

func hasTerraformFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".tf" {
			return true
		}
	}
	return false
}

func (app *App) RunInfraDeploy(ctx context.Context, tfDir string, skipPlan bool) error {
	box := logging.NewBox(app.Session.Console, app.Cfg.NoColor)
	box.Header("INFRASTRUCTURE DEPLOY")
	box.Row("Directory", tfDir)
	box.Row("Mode", app.deployModeLabel())
	box.Footer()

	// Preflight: terraform in PATH
	if _, err := terraform.LookPath(); err != nil {
		return fmt.Errorf("terraform not found in PATH: %w", err)
	}

	// Preflight: terraform.tfvars exists
	// Use the configured tfvars path (may be a scenario file via --tfvars flag)
	tfvarsPath := app.Cfg.TerraformTFVars
	if !filepath.IsAbs(tfvarsPath) {
		tfvarsPath = filepath.Join(tfDir, tfvarsPath)
	}
	if _, err := os.Stat(tfvarsPath); err != nil {
		return fmt.Errorf("tfvars file not found: %s", tfvarsPath)
	}

	runner := terraform.NewRunner(tfDir, app.Logger)
	if app.Session != nil {
		runner.SetOutput(app.Session.Console)
	}

	// Backup
	if !app.Cfg.SkipBackup {
		app.backupFile(tfDir, "terraform.tfstate", "tfstate")
		app.backupFile(tfDir, "terraform.tfvars", "tfvars")
	}

	// Init
	app.Logger.Info("initializing terraform")
	terraformDir := filepath.Join(tfDir, ".terraform")
	if _, err := os.Stat(terraformDir); os.IsNotExist(err) {
		if err := runner.Init(ctx); err != nil {
			return err
		}
	} else {
		app.Logger.Debug("terraform already initialized")
	}

	// Fmt + Validate
	app.Logger.Info("formatting and validating")
	if err := runner.Fmt(ctx); err != nil {
		app.Logger.Warn("terraform fmt failed", zap.Error(err))
	}
	if err := runner.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// ISO check (non-fatal)
	app.checkProxmoxISO(ctx, tfDir)

	// Plan
	app.Logger.Info("creating plan")
	planFile := filepath.Join(tfDir, fmt.Sprintf("tfplan-%d", time.Now().Unix()))
	defer os.Remove(planFile)

	varFileArg := fmt.Sprintf("-var-file=%s", tfvarsPath)
	hasChanges, err := runner.Plan(ctx, planFile, varFileArg)
	if err != nil {
		return err
	}

	if !hasChanges {
		app.Logger.Info("no changes needed - infrastructure is up to date")
		return nil
	}

	// Show plan summary
	if !skipPlan {
		planOutput, err := runner.ShowJSON(ctx, planFile)
		if err != nil {
			app.Logger.Warn("could not parse plan JSON", zap.Error(err))
		} else {
			app.displayInfraPlanSummary(app.Session.Console, planOutput)
		}
	}

	// Dry run exit
	if app.Cfg.DryRun {
		app.Logger.Info("dry run complete", zap.String("plan_file", planFile))
		return nil
	}

	// Confirm
	if !app.Cfg.AutoApprove {
		if !app.PromptConfirm("Proceed with deployment? [y/N]: ") {
			os.Remove(planFile)
			return nil
		}
	} else {
		app.Logger.Info("auto-approving deployment")
	}

	// Apply with retry
	app.Logger.Info("applying changes")
	if err := runner.ApplyWithRetry(ctx, planFile, 3, 5*time.Second); err != nil {
		return err
	}

	// Save deploy state
	app.saveInfraState(ctx, tfDir, runner)

	// Show deployment summary
	stateOutput, err := runner.ShowStateJSON(ctx)
	if err != nil {
		app.Logger.Warn("could not read state for summary", zap.Error(err))
	} else {
		app.displayDeploySummary(app.Session.Console, stateOutput)
	}

	app.Logger.Info("deployment complete")
	return nil
}

func (app *App) RunInfraDestroy(ctx context.Context, tfDir string, force, graceful bool) error {
	box := logging.NewBox(app.Session.Console, app.Cfg.NoColor)
	if force {
		box.Header("INFRASTRUCTURE DESTROY [FORCE]")
		box.Badge("WARNING", "Force mode: bypassing safety checks")
	} else {
		box.Header("INFRASTRUCTURE DESTROY")
	}
	box.Row("Directory", tfDir)
	box.Footer()

	if force {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		app.Logger.Warn("force mode: 120s timeout active")
	}

	runner := terraform.NewRunner(tfDir, app.Logger)
	if app.Session != nil {
		runner.SetOutput(app.Session.Console)
	}

	// Init
	app.Logger.Info("initializing terraform")
	if err := runner.Init(ctx); err != nil {
		return err
	}

	// Check state
	tfstatePath := filepath.Join(tfDir, "terraform.tfstate")
	if _, err := os.Stat(tfstatePath); err != nil {
		app.Logger.Warn("no state file found, nothing to destroy")
		return nil
	}

	resources, err := runner.StateList(ctx)
	if err != nil {
		if force {
			app.Logger.Warn("could not list state resources", zap.Error(err))
		} else {
			return fmt.Errorf("state list: %w", err)
		}
	}

	if len(resources) == 0 {
		app.Logger.Info("no resources in state")
		return nil
	}

	// Show resources
	app.Logger.Info("resources to destroy", zap.Int("count", len(resources)))
	for _, r := range resources {
		fmt.Fprintf(app.Session.Console, "  - %s\n", r)
	}

	// Check for active cluster (unless force)
	if !force {
		talosConfigDir := filepath.Join(tfDir, "..", "config")
		talosConfigPath := filepath.Join(talosConfigDir, "talosconfig")
		kubeconfigPath := filepath.Join(talosConfigDir, "kubeconfig")
		if fileExists(talosConfigPath) && fileExists(kubeconfigPath) {
			app.Logger.Warn("active Kubernetes cluster detected",
				zap.String("config_dir", talosConfigDir))
			fmt.Fprintln(app.Session.Console, "\nPre-destruction checklist:")
			fmt.Fprintln(app.Session.Console, "  1. kubectl drain <node> --ignore-daemonsets --delete-emptydir-data")
			fmt.Fprintln(app.Session.Console, "  2. kubectl delete node <node>")
			fmt.Fprintln(app.Session.Console, "  3. talosctl etcd snapshot backup.yaml")
			fmt.Fprintln(app.Session.Console)
		}
	}

	// Backup
	if !app.Cfg.SkipBackup {
		app.backupFile(tfDir, "terraform.tfstate", "pre-destroy")
	}

	// Create destroy plan
	app.Logger.Info("creating destruction plan")
	planFile := filepath.Join(tfDir, "tfdestroy-plan")
	defer os.Remove(planFile)

	var tfOpts []string
	if force {
		tfOpts = append(tfOpts, "-refresh=false")
	}

	if err := runner.DestroyPlan(ctx, planFile, tfOpts...); err != nil {
		if !force {
			return err
		}
		app.Logger.Warn("destroy plan failed", zap.Error(err))
	}

	// Show destroy count
	if _, statErr := os.Stat(planFile); statErr == nil {
		planOutput, err := runner.ShowJSON(ctx, planFile)
		if err == nil {
			app.Logger.Warn("will destroy resources", zap.Int("count", len(planOutput.ResourceChanges)))
		}
	}

	// Confirmation
	if !app.Cfg.AutoApprove && !force {
		fmt.Fprintln(app.Session.Console, "\nFINAL WARNING - THIS CANNOT BE UNDONE")
		fmt.Fprint(app.Session.Console, "Type \"DESTROY\" (all caps) to confirm: ")
		var confirmation string
		fmt.Scanln(&confirmation)
		fmt.Fprintln(app.Session.ConsoleFile, confirmation)
		if confirmation != "DESTROY" {
			app.Logger.Info("cancelled by user")
			return nil
		}
	} else if force {
		app.Logger.Warn("force mode: skipping confirmation")
	} else {
		app.Logger.Info("auto-approving destruction")
	}

	// Graceful VM shutdown
	if graceful && !force {
		app.shutdownVMs(ctx, tfDir, runner, resources)
	}

	// Apply destroy
	app.Logger.Info("destroying resources")
	if _, statErr := os.Stat(planFile); statErr == nil {
		applyArgs := []string{"-auto-approve"}
		if force {
			applyArgs = append(applyArgs, "-refresh=false")
		}
		if err := runner.Apply(ctx, planFile, applyArgs...); err != nil {
			if force {
				app.Logger.Warn("plan apply failed, trying direct destroy", zap.Error(err))
				if err := runner.Destroy(ctx, tfOpts...); err != nil {
					return fmt.Errorf("terraform destroy failed: %w", err)
				}
			} else {
				return err
			}
		}
	} else if force {
		if err := runner.Destroy(ctx, tfOpts...); err != nil {
			return fmt.Errorf("terraform destroy failed: %w", err)
		}
	}

	// Remove state if empty
	if resources, err := runner.StateList(ctx); err == nil && len(resources) == 0 {
		os.Remove(tfstatePath)
		stateDir := filepath.Join(tfDir, ".tf-deploy-state")
		os.Remove(filepath.Join(stateDir, "deploy-state.json"))
		app.Logger.Info("all resources destroyed")
	} else if err == nil {
		app.Logger.Warn("some resources may remain", zap.Int("count", len(resources)))
	}

	app.Logger.Info("destruction complete")
	return nil
}

func (app *App) RunInfraPlan(ctx context.Context, tfDir string) error {
	box := logging.NewBox(app.Session.Console, app.Cfg.NoColor)
	box.Header("INFRASTRUCTURE PLAN")
	box.Row("Directory", tfDir)
	box.Footer()

	if _, err := terraform.LookPath(); err != nil {
		return fmt.Errorf("terraform not found in PATH: %w", err)
	}

	runner := terraform.NewRunner(tfDir, app.Logger)

	// Init
	terraformDir := filepath.Join(tfDir, ".terraform")
	if _, err := os.Stat(terraformDir); os.IsNotExist(err) {
		app.Logger.Info("initializing terraform")
		if err := runner.Init(ctx); err != nil {
			return err
		}
	}

	// Validate
	if err := runner.Fmt(ctx); err != nil {
		app.Logger.Warn("terraform fmt failed", zap.Error(err))
	}
	if err := runner.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Plan - use configured tfvars path (may be a scenario file)
	tfvarsPath := app.Cfg.TerraformTFVars
	if !filepath.IsAbs(tfvarsPath) {
		tfvarsPath = filepath.Join(tfDir, tfvarsPath)
	}
	planFile := filepath.Join(tfDir, "tfplan-view")
	varFileArg := fmt.Sprintf("-var-file=%s", tfvarsPath)
	hasChanges, err := runner.Plan(ctx, planFile, varFileArg)
	if err != nil {
		return err
	}
	defer os.Remove(planFile)

	if !hasChanges {
		app.Logger.Info("no changes needed - infrastructure is up to date")
		return nil
	}

	planOutput, err := runner.ShowJSON(ctx, planFile)
	if err != nil {
		return fmt.Errorf("parse plan: %w", err)
	}

	app.displayInfraPlanSummary(app.Session.Console, planOutput)
	return nil
}

func (app *App) RunInfraStatus(tfDir string) error {
	box := logging.NewBox(app.Session.Console, app.Cfg.NoColor)
	box.Header("INFRASTRUCTURE STATUS")

	// Read deploy state (no terraform binary needed)
	stateDir := filepath.Join(tfDir, ".tf-deploy-state")
	deployStatePath := filepath.Join(stateDir, "deploy-state.json")
	if data, err := os.ReadFile(deployStatePath); err == nil {
		var deployState types.InfraDeployState
		if err := json.Unmarshal(data, &deployState); err == nil {
			box.Section("Last Deployment")
			box.Row("Timestamp", deployState.Timestamp)
			box.Row("Terraform Version", deployState.TerraformVersion)
			box.Row("Auto Approved", fmt.Sprintf("%v", deployState.AutoApproved))
			box.Row("Last Deployment", deployState.LastDeployment)
		}
	} else {
		box.Badge("NONE", "No deployment state found")
	}

	// Read terraform.tfstate directly with encoding/json
	tfstatePath := filepath.Join(tfDir, "terraform.tfstate")
	if data, err := os.ReadFile(tfstatePath); err == nil {
		var tfstate struct {
			Resources []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(data, &tfstate); err == nil {
			count := len(tfstate.Resources)
			box.Section("Terraform State")
			box.Row("Managed Resources", fmt.Sprintf("%d", count))

			if count > 0 {
				shown := 0
				for _, r := range tfstate.Resources {
					if shown >= 10 {
						box.Item("...", fmt.Sprintf("and %d more", count-10))
						break
					}
					box.Item("-", fmt.Sprintf("%s.%s", r.Type, r.Name))
					shown++
				}
			}
		}
	} else {
		box.Section("Terraform State")
		box.Badge("NONE", "No terraform.tfstate found")
	}

	box.Footer()
	return nil
}

func (app *App) RunInfraCleanup(tfDir string, all bool) error {
	app.Logger.Info("cleaning up generated files", zap.String("dir", tfDir))

	patterns := []string{
		filepath.Join(tfDir, "tfplan*"),
		filepath.Join(tfDir, ".terraform.lock.hcl"),
		filepath.Join(tfDir, "crash.log"),
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if err := os.Remove(match); err == nil {
				app.Logger.Info("removed", zap.String("file", match))
			}
		}
	}

	if all {
		dirs := []string{
			filepath.Join(tfDir, ".terraform"),
			filepath.Join(tfDir, "backups"),
			filepath.Join(tfDir, ".tf-deploy-state"),
		}
		for _, dir := range dirs {
			if err := os.RemoveAll(dir); err == nil {
				app.Logger.Info("removed", zap.String("dir", dir))
			}
		}
	}

	app.Logger.Info("cleanup complete")
	return nil
}

func (app *App) shutdownVMs(ctx context.Context, tfDir string, runner *terraform.Runner, resources []string) {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Warn("graceful shutdown: could not resolve tfvars path", zap.Error(err))
		return
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Warn("graceful shutdown: could not load terraform extras", zap.Error(err))
		return
	}

	if cfg.ProxmoxSSHHost == "" || len(cfg.ProxmoxNodeIPs) == 0 {
		app.Logger.Warn("cannot perform graceful shutdown: Proxmox host not configured")
		return
	}

	// Find VM resources and extract VMIDs from state
	var vmResources []string
	for _, r := range resources {
		if strings.Contains(r, "proxmox_virtual_environment_vm") {
			vmResources = append(vmResources, r)
		}
	}

	if len(vmResources) == 0 {
		app.Logger.Debug("no VM resources found in state, skipping graceful shutdown")
		return
	}

	app.Logger.Info("stopping VMs gracefully", zap.Int("count", len(vmResources)))

	// Always use insecure SSH for `qm stop` - matching bash behavior
	scanner := discovery.NewScanner(cfg.ProxmoxSSHUser, cfg.ProxmoxNodeIPs, true)
	defer scanner.Close()

	if cfg.ProxmoxSSHKeyPath != "" {
		if err := scanner.SetPrivateKey(cfg.ProxmoxSSHKeyPath); err != nil {
			app.Logger.Warn("failed to set SSH key for VM shutdown", zap.Error(err))
			return
		}
	}

	// Get state details for each VM to find VMID
	stateOutput, err := runner.ShowStateJSON(ctx)
	if err != nil {
		app.Logger.Warn("could not read state for VM shutdown", zap.Error(err))
		return
	}

	if stateOutput.Values == nil || stateOutput.Values.RootModule == nil {
		return
	}

	for _, r := range stateOutput.Values.RootModule.Resources {
		if r.Type != "proxmox_virtual_environment_vm" {
			continue
		}

		vmIDFloat, ok := r.Values["vm_id"].(float64)
		if !ok || vmIDFloat == 0 {
			continue
		}
		vmID := int(vmIDFloat)

		nodeName, _ := r.Values["node_name"].(string)
		if nodeName == "" {
			nodeName = "pve1"
		}

		nodeIP, ok := cfg.ProxmoxNodeIPs[nodeName]
		if !ok {
			nodeIP = net.ParseIP(cfg.ProxmoxSSHHost)
		}
		if nodeIP == nil {
			continue
		}

		app.Logger.Info("stopping VM", zap.Int("vmid", vmID), zap.String("node", nodeName))
		if err := scanner.StopVM(ctx, vmID, nodeIP); err != nil {
			app.Logger.Warn("failed to stop VM", zap.Int("vmid", vmID), zap.Error(err))
		}
	}

	app.Logger.Info("waiting for VMs to stop")
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
}

func (app *App) deployModeLabel() string {
	if app.Cfg.DryRun {
		return "DRY RUN"
	}
	return "DEPLOY"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (app *App) backupFile(tfDir, filename, prefix string) {
	src := filepath.Join(tfDir, filename)
	if _, err := os.Stat(src); err != nil {
		return
	}

	backupDir := filepath.Join(tfDir, "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		app.Logger.Warn("could not create backup dir", zap.Error(err))
		return
	}

	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s-%s.%s",
		prefix, time.Now().Format("20060102_150405"), filename))

	data, err := os.ReadFile(src)
	if err != nil {
		app.Logger.Warn("could not read file for backup", zap.String("file", src), zap.Error(err))
		return
	}

	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		app.Logger.Warn("could not write backup", zap.String("file", backupPath), zap.Error(err))
		return
	}

	app.Logger.Debug("backed up", zap.String("file", backupPath))
}

func (app *App) saveInfraState(ctx context.Context, tfDir string, runner *terraform.Runner) {
	stateDir := filepath.Join(tfDir, ".tf-deploy-state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		app.Logger.Warn("could not create state dir", zap.Error(err))
		return
	}

	tfVersion, _ := runner.Version(ctx)

	deployState := types.InfraDeployState{
		Timestamp:        time.Now().Format(time.RFC3339),
		TerraformVersion: tfVersion,
		AutoApproved:     app.Cfg.AutoApprove,
		LastDeployment:   time.Now().Format("2006-01-02 15:04:05"),
	}

	data, err := json.MarshalIndent(deployState, "", "  ")
	if err != nil {
		app.Logger.Warn("could not marshal deploy state", zap.Error(err))
		return
	}

	statePath := filepath.Join(stateDir, "deploy-state.json")
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		app.Logger.Warn("could not save deploy state", zap.Error(err))
	}
}

func (app *App) displayInfraPlanSummary(w io.Writer, plan *terraform.PlanOutput) {
	summary := terraform.SummarizePlan(plan)

	box := logging.NewBox(w, app.Cfg.NoColor)
	box.Header("PLAN SUMMARY")

	if summary.Create > 0 {
		box.Item("+", fmt.Sprintf("Create: %d", summary.Create))
	}
	if summary.Update > 0 {
		box.Item("~", fmt.Sprintf("Update: %d", summary.Update))
	}
	if summary.Delete > 0 {
		box.Item("-", fmt.Sprintf("Delete: %d", summary.Delete))
	}
	if summary.Replace > 0 {
		box.Item("!", fmt.Sprintf("Replace: %d", summary.Replace))
	}

	if len(summary.Changes) > 0 {
		box.Section("Details")
		for _, rc := range summary.Changes {
			action := strings.ToUpper(strings.Join(rc.Change.Actions, "/"))
			box.Item(action, fmt.Sprintf("%s.%s", rc.Type, rc.Name))
		}
	}

	box.Footer()
}

func (app *App) displayDeploySummary(w io.Writer, stateOutput *terraform.StateOutput) {
	box := logging.NewBox(w, app.Cfg.NoColor)
	box.Header("DEPLOYMENT SUMMARY")

	if stateOutput.Values == nil || stateOutput.Values.RootModule == nil {
		box.Badge("OK", "Deployment complete")
		box.Footer()
		return
	}

	for _, r := range stateOutput.Values.RootModule.Resources {
		if r.Type == "proxmox_virtual_environment_vm" {
			name, _ := r.Values["name"].(string)
			vmID, _ := r.Values["vm_id"].(float64)
			detail := fmt.Sprintf("%s (VMID: %.0f)", name, vmID)
			box.Item("•", detail)
		}
	}

	box.Section("Next Steps")
	box.Item("1", "Verify VMs: terraform show")
	box.Item("2", "Bootstrap cluster: talops bootstrap")
	box.Item("3", "Check status: talops infra status")
	box.Footer()
}

func (app *App) checkProxmoxISO(ctx context.Context, tfDir string) {
	cfg := app.Cfg
	stateMgr := state.NewManager(cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Debug("ISO check: could not resolve tfvars path", zap.Error(err))
		return
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		return
	}

	if cfg.ProxmoxTokenID == "" || cfg.ProxmoxTokenSecret == "" || cfg.ProxmoxSSHHost == "" {
		return
	}

	// Read ISO name from tfvars
	tfvarsData, err := os.ReadFile(filepath.Join(tfDir, "terraform.tfvars"))
	if err != nil {
		return
	}
	content := string(tfvarsData)

	isoName := ExtractTFVarString(content, "talos_iso")
	if isoName == "" {
		return
	}

	app.Logger.Debug("checking Proxmox ISO", zap.String("iso", isoName))

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	nodeName := "pve1"
	for name := range cfg.ProxmoxNodeIPs {
		nodeName = name
		break
	}
	apiURL := fmt.Sprintf("https://%s:8006/api2/json/nodes/%s/storage/local/content", cfg.ProxmoxSSHHost, nodeName)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", cfg.ProxmoxTokenID, cfg.ProxmoxTokenSecret))

	resp, err := client.Do(req)
	if err != nil {
		app.Logger.Debug("ISO check failed (Proxmox unreachable)", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if strings.Contains(string(body), isoName) {
		app.Logger.Info("ISO found on Proxmox", zap.String("iso", isoName))
	} else {
		app.Logger.Warn("ISO not found on Proxmox (will continue anyway)", zap.String("iso", isoName))
	}
}

// ExtractTFVarString extracts a simple string value from tfvars content.
// Uses exact key matching (before "=") to avoid prefix collisions.
func ExtractTFVarString(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		stripped := strings.TrimSpace(line)
		eqIdx := strings.Index(stripped, "=")
		if eqIdx > 0 && strings.TrimSpace(stripped[:eqIdx]) == key {
			val := strings.TrimSpace(stripped[eqIdx+1:])
			val = strings.Trim(val, "\"")
			return val
		}
	}
	return ""
}
