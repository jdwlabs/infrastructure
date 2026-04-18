package app

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// App holds the application state that was previously stored in package-level globals.
// All orchestration handlers are methods on App.
type App struct {
	Cfg     *types.Config
	Logger  *zap.Logger
	Session *logging.RunSession
	Version string
}

// New creates a new App with default config.
func New(version string) *App {
	return &App{
		Cfg:     types.DefaultConfig(),
		Version: version,
	}
}

// Close closes the session if it exists.
func (app *App) Close(err error) {
	if app.Session != nil {
		app.Session.Close(err)
		app.Session = nil
	}
}

func (app *App) InitSession(cmd *cobra.Command) error {
	var err error
	app.Session, err = logging.NewRunSession(app.Cfg)
	if err != nil {
		return fmt.Errorf("initialize logging session: %w", err)
	}
	app.Logger = app.Session.Logger

	logging.PrintBanner(app.Session.Console, app.Version, app.Cfg.NoColor)

	app.CheckPrerequisites()

	if cmd.Parent() == nil || cmd.Parent().Name() != "infra" {
		clusterDir := filepath.Join("clusters", app.Cfg.ClusterName)
		EnsureClusterGitignore(clusterDir)
	}

	return nil
}

func (app *App) InitConfig(_ *cobra.Command) error {
	cfg := app.Cfg
	if v := os.Getenv("CLUSTER_NAME"); v != "" {
		cfg.ClusterName = v
		cfg.SecretsDir = filepath.Join("clusters", cfg.ClusterName, "secrets")
	}
	if v := os.Getenv("TERRAFORM_TFVARS"); v != "" {
		cfg.TerraformTFVars = v
	}
	if v := os.Getenv("CONTROL_PLANE_ENDPOINT"); v != "" {
		cfg.ControlPlaneEndpoint = v
	}
	if v := os.Getenv("HAPROXY_IP"); v != "" {
		cfg.HAProxyIP = net.ParseIP(v)
	}
	if v := os.Getenv("KUBERNETES_VERSION"); v != "" {
		cfg.KubernetesVersion = v
	}
	if v := os.Getenv("TALOS_VERSION"); v != "" {
		cfg.TalosVersion = v
	}
	if v := os.Getenv("SECRETS_DIR"); v != "" {
		cfg.SecretsDir = v
	}
	if v := os.Getenv("SSH_KEY_PATH"); v != "" {
		cfg.ProxmoxSSHKeyPath = v
	}
	if v := os.Getenv("INSTALLER_IMAGE"); v != "" {
		cfg.InstallerImage = v
	}
	if v := os.Getenv("HAPROXY_LOGIN_USER"); v != "" {
		cfg.HAProxyLoginUser = v
	}
	if v := os.Getenv("HAPROXY_STATS_USER"); v != "" {
		cfg.HAProxyStatsUser = v
	}
	if v := os.Getenv("HAPROXY_STATS_PASSWORD"); v != "" {
		cfg.HAProxyStatsPassword = v
	}
	if v := os.Getenv("INGRESS_HTTP_NODEPORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.IngressHTTPNodePort = n
		}
	}
	if v := os.Getenv("INGRESS_TLS_NODEPORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.IngressTLSNodePort = n
		}
	}

	return nil
}

// PromptConfirm writes a prompt to session.Console, reads a y/N response from
// stdin, and logs the result. Returns true if the user confirmed.
func (app *App) PromptConfirm(prompt string) bool {
	_, _ = fmt.Fprint(app.Session.Console, prompt)
	var response string
	_, _ = fmt.Scanln(&response)
	_, _ = fmt.Fprintln(app.Session.ConsoleFile, response)
	if response != "y" && response != "Y" {
		app.Session.Logger.Warn("cancelled by user", zap.String("response", response))
		return false
	}
	app.Session.Logger.Info("confirmed by user", zap.String("response", response))
	return true
}

// ResolveAllPaths resolves TerraformDir and TerraformTFVars to absolute paths
// once early in the command lifecycle. This prevents double-joining and
// re-resolution bugs across the lifecycle.
func (app *App) ResolveAllPaths() {
	// Step 1: Resolve TerraformDir to absolute (needed as a candidate f or tfvars search).
	if _, err := app.ResolveTerraformDir(); err != nil {
		if app.Logger != nil {
			app.Logger.Debug("terraform directory not found during path resolution", zap.Error(err))
		}
	}

	// Step 2: Resolve TerraformTFVars to absolute.
	stateMgr := state.NewManager(app.Cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		if app.Logger != nil {
			app.Logger.Debug("terraform.tfvars not found during path resolution", zap.Error(err))
		}
	}
}

// CheckPrerequisites verifies required CLI tools are available
func (app *App) CheckPrerequisites() {
	for _, tool := range []string{"talosctl", "kubectl", "terraform"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			app.Logger.Warn("prerequisite not found in PATH", zap.String("tool", tool))
			continue
		}
		cmd := exec.Command(tool, "version", "--client")
		out, err := cmd.CombinedOutput()
		if err != nil {
			cmd = exec.Command(tool, "version")
			out, _ = cmd.CombinedOutput()
		}
		ver := strings.TrimSpace(strings.Split(string(out), "\n")[0])
		app.Logger.Debug("prerequisite found", zap.String("tool", tool), zap.String("path", path), zap.String("version", ver))
	}
}

// EnsureClusterGitignore creates a .gitignore in the cluster directory
// to prevent committing generated secrets, node configs, state, and logs.
func EnsureClusterGitignore(clusterDir string) {
	gitignorePath := filepath.Join(clusterDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); err == nil {
		return // already exists
	}
	if err := os.MkdirAll(clusterDir, 0755); err != nil {
		return
	}
	content := "/nodes/\n/secrets/\n/state/\n/*.log\n"
	_ = os.WriteFile(gitignorePath, []byte(content), 0644)
}
