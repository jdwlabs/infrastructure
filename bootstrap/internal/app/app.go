package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/secrets"
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

	// Vault manages the encrypted SOPS secret store. vaultActive is true when
	// sops is available and the vault is in use (false = legacy plaintext mode).
	// readOnlyCmd suppresses the post-run auto-seal for commands that do not
	// mutate secrets (status/plan/version and the self-managing `secrets` group).
	Vault       *secrets.Vault
	vaultActive bool
	readOnlyCmd bool
}

// EnvAutoLock, when set, makes talops wipe the plaintext working copies after a
// successful run (the encrypted vault remains). Off by default to keep the
// plaintext available for inspection while debugging.
const EnvAutoLock = "TALOPS_AUTOLOCK"

// New creates a new App with default config.
func New(version string) *App {
	return &App{
		Cfg:     types.DefaultConfig(),
		Version: version,
	}
}

// Close seals the vault on success and closes the session if it exists.
func (app *App) Close(err error) {
	if app.Session != nil {
		app.autoSeal(err)
		app.Session.Close(err)
		app.Session = nil
	}
}

// autoSeal re-encrypts changed plaintext working files back into the vault after
// a successful command, unless disabled via TALOPS_NO_AUTOSEAL or the command is
// read-only. Failures here are logged but never mask the command's own result.
// When TALOPS_AUTOLOCK is set, the plaintext working copies are wiped afterward.
func (app *App) autoSeal(runErr error) {
	if !app.vaultActive || runErr != nil || app.readOnlyCmd {
		return
	}
	if os.Getenv(secrets.EnvNoAutoSeal) != "" {
		app.Logger.Debug("auto-seal disabled via " + secrets.EnvNoAutoSeal)
		return
	}
	sealed, err := app.Vault.Seal(context.Background())
	if err != nil {
		app.Logger.Warn("failed to seal secrets into vault; plaintext changes are not yet encrypted",
			zap.Error(err))
		return
	}
	if len(sealed) > 0 {
		app.Logger.Info("sealed updated secrets into vault — commit the .enc.yaml changes to share them",
			zap.Int("count", len(sealed)))
	}
	if os.Getenv(EnvAutoLock) != "" {
		if err := app.Vault.WipePlaintext(); err != nil {
			app.Logger.Warn("auto-lock: failed to remove plaintext working copies", zap.Error(err))
		}
	}
}

// MarkReadOnly flags whether the invoked command mutates secrets, controlling
// the post-run auto-seal. The `secrets` subcommands manage the vault explicitly,
// and status/plan/version never change it.
func (app *App) MarkReadOnly(cmd *cobra.Command) {
	name := cmd.Name()
	parent := ""
	if cmd.Parent() != nil {
		parent = cmd.Parent().Name()
	}
	app.readOnlyCmd = parent == "secrets" ||
		name == "status" || name == "plan" || name == "version"
}

// AnchorToRepoRoot changes the working directory to the repository root so that
// all cwd-relative paths (clusters/, terraform/, the vault) resolve to the same
// place regardless of where talops is invoked from. Without this, running from
// the repo root vs. bootstrap/ would create two divergent vaults. Path flags the
// user set explicitly are made absolute first so they survive the chdir.
func (app *App) AnchorToRepoRoot(cmd *cobra.Command) error {
	root := secrets.RepoRoot()
	if root == "" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == root {
		return nil
	}
	flagPaths := map[string]*string{
		"tfvars":          &app.Cfg.TerraformTFVars,
		"terraform-dir":   &app.Cfg.TerraformDir,
		"ssh-key":         &app.Cfg.ProxmoxSSHKeyPath,
		"haproxy-ssh-key": &app.Cfg.HAProxySSHKeyPath,
		"log-dir":         &app.Cfg.LogDir,
		"patch-dir":       &app.Cfg.PatchDir,
	}
	for name, ptr := range flagPaths {
		if cmd.Flags().Changed(name) && *ptr != "" && !filepath.IsAbs(*ptr) {
			if abs, absErr := filepath.Abs(*ptr); absErr == nil {
				*ptr = abs
			}
		}
	}
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("anchor to repo root %s: %w", root, err)
	}
	fmt.Fprintf(os.Stderr, "talops: running from repo root %s\n", root)
	return nil
}

// HydrateSecrets decrypts the committed vault into the plaintext working files
// before a command runs. It is a no-op (legacy plaintext mode) when sops is not
// installed, so the tool still works on machines without the vault tooling.
//
// Order matters: terraform.tfvars is hydrated first so the cluster name can be
// resolved from it before the per-cluster vault (clusters/<name>/vault) paths
// are computed — otherwise hydrate and the post-run seal could disagree on the
// cluster directory.
func (app *App) HydrateSecrets() error {
	if app.Vault == nil {
		return nil
	}
	if err := app.Vault.Available(); err != nil {
		app.Logger.Warn("sops not available — operating on plaintext only; encrypted vault disabled",
			zap.Error(err))
		app.vaultActive = false
		return nil
	}
	app.vaultActive = true
	ctx := context.Background()

	keyChecked := false
	requireKey := func() error {
		if keyChecked {
			return nil
		}
		keyChecked = true
		return app.Vault.KeyAvailable()
	}

	// 1. tfvars first (not cluster-specific).
	if _, err := os.Stat(app.Vault.TFVarsEntry().Enc); err == nil {
		if err := requireKey(); err != nil {
			return err
		}
		if _, err := app.Vault.HydrateTFVars(ctx); err != nil {
			return err
		}
	}

	// 2. Resolve the cluster name (and SecretsDir) from tfvars so the
	//    per-cluster vault paths below match what the command will use.
	stateMgr := state.NewManager(app.Cfg, app.Logger)
	if err := stateMgr.ResolveTFVarsPath(); err != nil {
		app.Logger.Debug("tfvars not found during secret hydration", zap.Error(err))
	}
	if err := stateMgr.LoadTerraformExtras(ctx); err != nil {
		app.Logger.Debug("could not load terraform extras during secret hydration", zap.Error(err))
	}

	// 3. Now the per-cluster artifacts.
	for _, e := range app.Vault.ClusterEntries() {
		if _, err := os.Stat(e.Enc); err == nil {
			if err := requireKey(); err != nil {
				return err
			}
			break
		}
	}
	if _, err := app.Vault.HydrateCluster(ctx); err != nil {
		return err
	}
	return nil
}

// CheckVaultGit enforces multi-device safety: it refuses to operate when the
// local branch is behind its upstream (a likely-stale vault) unless overridden,
// and warns about uncommitted vault changes left over from a previous run. When
// fetch is true it runs `git fetch` first so the behind-count is accurate.
func (app *App) CheckVaultGit(allowStale, fetch bool) error {
	if !app.vaultActive || !app.Vault.HasEncrypted() {
		return nil
	}
	ctx := context.Background()
	if fetch {
		if err := secrets.FetchUpstream(ctx); err != nil {
			app.Logger.Warn("git fetch failed; stale-vault check uses last-known upstream", zap.Error(err))
		}
	}
	st := secrets.VaultGitState(ctx, app.Vault.EncPaths())
	if !st.IsRepo {
		return nil
	}
	if len(st.DirtyVault) > 0 {
		app.Logger.Warn("uncommitted vault changes from a previous run — commit and push them to share",
			zap.Int("files", len(st.DirtyVault)))
	}
	if st.Behind > 0 {
		if allowStale {
			app.Logger.Warn("local branch is behind upstream; vault may be stale (continuing due to --allow-stale-vault)",
				zap.Int("behind", st.Behind))
			return nil
		}
		return fmt.Errorf("local branch is %d commit(s) behind upstream: the encrypted vault may be stale. "+
			"Run `git pull` first, or pass --allow-stale-vault to override", st.Behind)
	}
	return nil
}

func (app *App) InitSession(cmd *cobra.Command) error {
	var err error
	app.Session, err = logging.NewRunSession(app.Cfg)
	if err != nil {
		return fmt.Errorf("initialize logging session: %w", err)
	}
	app.Logger = app.Session.Logger
	app.Vault = secrets.New(app.Cfg, app.Logger)

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
	}
	// ClusterName is final here (default < --cluster flag < CLUSTER_NAME env).
	// Re-derive SecretsDir from it: the flag writes ClusterName after
	// DefaultConfig precomputed the path, which once sent secrets for a named
	// cluster to clusters/cluster/ and generated a rogue CA there.
	// SECRETS_DIR below still wins as an explicit override.
	cfg.SecretsDir = filepath.Join("clusters", cfg.ClusterName, "secrets")
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
	if v := os.Getenv("ADMIN_ALLOWED_CIDRS"); v != "" {
		cfg.AdminAllowedCIDRs = strings.Split(v, ",")
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
	for _, tool := range []string{"talosctl", "kubectl", "terraform", "sops"} {
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
