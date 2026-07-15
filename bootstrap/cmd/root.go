package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/state"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var version = "dev"

// Execute builds the root command, runs it, and returns any error.
func Execute() error {
	a := app.New(version)

	var allowStaleVault bool
	var fetchVault bool
	var runErr error
	defer func() {
		a.Close(runErr)
	}()

	rootCmd := &cobra.Command{
		Use:          "talops",
		Short:        "Smart reconciliation for Talos clusters",
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Anchor to the repo root first so clusters/, terraform/, and the
			// vault resolve identically regardless of the invocation directory.
			if err := a.AnchorToRepoRoot(cmd); err != nil {
				return err
			}
			if err := a.InitConfig(cmd); err != nil {
				return err
			}
			if err := a.InitSession(cmd); err != nil {
				return err
			}
			// Decrypt the committed vault into the plaintext working files
			// before anything reads them (tfvars first, so path resolution
			// below can find a freshly hydrated terraform.tfvars).
			if err := a.HydrateSecrets(); err != nil {
				return err
			}
			a.MarkReadOnly(cmd)
			if err := a.CheckVaultGit(allowStaleVault, fetchVault); err != nil {
				return err
			}
			a.ResolveAllPaths()
			// Scaffold the per-cluster .gitignore only now that the cluster
			// name is final (flag/env/tfvars all merged).
			a.EnsureClusterScaffold(cmd)
			return nil
		},
	}

	cfg := a.Cfg

	// Global flags
	rootCmd.PersistentFlags().StringVarP(&cfg.ClusterName, "cluster", "c", types.DefaultClusterName, "Cluster name")
	rootCmd.PersistentFlags().StringVarP(&cfg.TerraformTFVars, "tfvars", "t", "terraform.tfvars", "Path to terraform.tfvars")
	rootCmd.PersistentFlags().BoolVarP(&cfg.AutoApprove, "auto-approve", "a", false, "Skip confirmations")
	rootCmd.PersistentFlags().BoolVarP(&cfg.DryRun, "dry-run", "d", false, "Simulate only")
	rootCmd.PersistentFlags().BoolVarP(&cfg.SkipPreflight, "skip-preflight", "s", false, "Skip connectivity checks")
	rootCmd.PersistentFlags().StringVarP(&cfg.LogLevel, "log-level", "l", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVarP(&cfg.ProxmoxSSHKeyPath, "ssh-key", "k", cfg.ProxmoxSSHKeyPath, "Path to SSH private key")
	rootCmd.PersistentFlags().BoolVarP(&cfg.ForceReconfigure, "force-reconfigure", "f", false, "Force reconfigure all nodes")
	rootCmd.PersistentFlags().StringVar(&cfg.LogDir, "log-dir", cfg.LogDir, "Log directory")
	rootCmd.PersistentFlags().BoolVar(&cfg.NoColor, "no-color", cfg.NoColor, "Disable colored output")
	rootCmd.PersistentFlags().StringVar(&cfg.ControlPlaneEndpoint, "control-plane-endpoint", "", "Control plane endpoint (e.g., cluster.example.com)")
	rootCmd.PersistentFlags().StringVar(&cfg.KubernetesVersion, "kubernetes-version", "", "Kubernetes version (e.g., v1.35.1)")
	rootCmd.PersistentFlags().StringVar(&cfg.TalosVersion, "talos-version", "", "Talos version (e.g., v1.12.13)")
	rootCmd.PersistentFlags().StringVar(&cfg.InstallerImage, "installer-image", "", "Talos installer image")
	rootCmd.PersistentFlags().StringVar(&cfg.HAProxyLoginUser, "haproxy-user", "", "HAProxy SSH login user")
	rootCmd.PersistentFlags().StringVar(&cfg.HAProxySSHKeyPath, "haproxy-ssh-key", "", "Path to SSH private key for HAProxy (defaults to --ssh-key)")
	rootCmd.PersistentFlags().StringVar(&cfg.HAProxyStatsUser, "haproxy-stats-user", "", "HAProxy stats username")
	rootCmd.PersistentFlags().StringVar(&cfg.HAProxyStatsPassword, "haproxy-stats-password", "", "HAProxy stats password")
	rootCmd.PersistentFlags().IntVar(&cfg.IngressHTTPNodePort, "ingress-http-nodeport", 0, "Ingress HTTP NodePort (HAProxy :80 backend)")
	rootCmd.PersistentFlags().IntVar(&cfg.IngressTLSNodePort, "ingress-tls-nodeport", 0, "Ingress TLS NodePort (HAProxy :443 backend)")
	rootCmd.PersistentFlags().BoolVar(&cfg.InsecureSSH, "insecure-ssh", false, "Skip SSH host key verification (INSECURE)")
	rootCmd.PersistentFlags().StringVar(&cfg.TerraformDir, "terraform-dir", "", "Directory containing Terraform files (default: auto-detect)")
	rootCmd.PersistentFlags().StringVar(&cfg.PatchDir, "patch-dir", "", "Directory for patch template overrides")
	rootCmd.PersistentFlags().BoolVar(&allowStaleVault, "allow-stale-vault", false, "Operate even if the local branch is behind upstream (vault may be stale)")
	rootCmd.PersistentFlags().BoolVar(&fetchVault, "fetch", false, "Run git fetch before the stale-vault check for an accurate behind-count")

	rootCmd.AddCommand(
		bootstrapCmd(a),
		reconcileCmd(a),
		statusCmd(a),
		resetCmd(a),
		infraCmd(a),
		upCmd(a),
		downCmd(a),
		pruneNodesCmd(a),
		secretsCmd(a),
		versionCmd(version),
	)

	runErr = rootCmd.Execute()
	a.Close(runErr)
	a.Session = nil // prevent double-close in defer
	if runErr != nil {
		return runErr
	}
	return nil
}

func bootstrapCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Initial cluster deployment",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return a.RunReconcile(ctx)
		},
	}
}

func reconcileCmd(a *app.App) *cobra.Command {
	var planMode bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile cluster with terraform.tfvars",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			if planMode {
				a.Cfg.PlanMode = true
				a.Cfg.DryRun = true
			}

			return a.RunReconcile(ctx)
		},
	}
	cmd.Flags().BoolVarP(&planMode, "plan", "p", false, "Show changes without applying")
	return cmd
}

func statusCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current cluster status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			return a.RunStatus(ctx)
		},
	}
}

func resetCmd(a *app.App) *cobra.Command {
	var purgeVault bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset cluster state",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load terraform extras so cluster name is resolved from tfvars
			stateMgr := state.NewManager(a.Cfg, a.Logger)
			if err := stateMgr.ResolveTFVarsPath(); err != nil {
				a.Logger.Warn("could not locate terraform.tfvars", zap.Error(err))
			}
			if err := stateMgr.LoadTerraformExtras(context.Background()); err != nil {
				a.Logger.Warn("could not load terraform extras", zap.String("path", a.Cfg.TerraformTFVars), zap.Error(err))
			}

			clusterDir := filepath.Join("clusters", a.Cfg.ClusterName)
			prompt := fmt.Sprintf("Are you sure you want to reset cluster %q (%s)? [y/N]: ", a.Cfg.ClusterName, clusterDir)
			if purgeVault {
				prompt = fmt.Sprintf("Reset cluster %q AND DELETE its encrypted vault (%s)? This destroys the only backup of the Talos secrets. [y/N]: ",
					a.Cfg.ClusterName, filepath.Join(clusterDir, "vault"))
			}
			if !a.Cfg.AutoApprove {
				if !a.PromptConfirm(prompt) {
					return nil
				}
			}

			// Remove the plaintext working state. The encrypted vault/ is
			// preserved unless --purge-vault is given, so a reset does not
			// destroy the recoverable secrets bundle.
			if purgeVault {
				if err := os.RemoveAll(clusterDir); err != nil {
					a.Logger.Error("Remove cluster failed", zap.Error(err))
					return fmt.Errorf("remove cluster dir: %w", err)
				}
			} else {
				entries, err := os.ReadDir(clusterDir)
				if err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("read cluster dir: %w", err)
				}
				for _, e := range entries {
					if e.Name() == "vault" {
						continue
					}
					path := filepath.Join(clusterDir, e.Name())
					if err := os.RemoveAll(path); err != nil {
						a.Logger.Error("Remove failed", zap.String("path", path), zap.Error(err))
						return fmt.Errorf("remove %s: %w", path, err)
					}
				}
			}
			a.Logger.Info("Reset cluster", zap.String("clusterDir", clusterDir), zap.Bool("purge_vault", purgeVault))
			return nil
		},
	}
	cmd.Flags().BoolVar(&purgeVault, "purge-vault", false, "Also delete the encrypted vault (destroys the secrets backup)")
	return cmd
}

func infraCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Manage infrastructure (Terraform VM provisioning)",
	}

	cmd.PersistentFlags().BoolVar(&a.Cfg.SkipBackup, "skip-backup", false, "Skip backup creation before operations")

	cmd.AddCommand(
		infraDeployCmd(a),
		infraDestroyCmd(a),
		infraPlanCmd(a),
		infraStatusCmd(a),
		infraCleanupCmd(a),
	)

	return cmd
}

func infraDeployCmd(a *app.App) *cobra.Command {
	var skipPlan bool

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy or update cluster infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			tfDir, err := a.ResolveTerraformDir()
			if err != nil {
				return err
			}
			return a.RunInfraDeploy(ctx, tfDir, skipPlan)
		},
	}

	cmd.Flags().BoolVar(&skipPlan, "skip-plan", false, "Skip detailed plan summary")

	return cmd
}

func infraDestroyCmd(a *app.App) *cobra.Command {
	var (
		force    bool
		graceful bool
	)

	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy cluster infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			tfDir, err := a.ResolveTerraformDir()
			if err != nil {
				return err
			}
			return a.RunInfraDestroy(ctx, tfDir, force, graceful)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Force mode (bypass safety checks, no refresh)")
	cmd.Flags().BoolVar(&graceful, "graceful", true, "Gracefully stop VMs before destroying")

	return cmd
}

func infraPlanCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Preview infrastructure changes without applying",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			tfDir, err := a.ResolveTerraformDir()
			if err != nil {
				return err
			}
			return a.RunInfraPlan(ctx, tfDir)
		},
	}
}

func infraStatusCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current infrastructure deployment state",
		RunE: func(cmd *cobra.Command, args []string) error {
			tfDir, err := a.ResolveTerraformDir()
			if err != nil {
				return err
			}
			return a.RunInfraStatus(tfDir)
		},
	}
}

func infraCleanupCmd(a *app.App) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove generated Terraform files",
		RunE: func(cmd *cobra.Command, args []string) error {
			tfDir, err := a.ResolveTerraformDir()
			if err != nil {
				return err
			}
			return a.RunInfraCleanup(tfDir, all)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Also remove .terraform/, backups/, and deploy state")

	return cmd
}

func upCmd(a *app.App) *cobra.Command {
	var skipInfra bool

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Provision infrastructure and bootstrap cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return a.RunUp(ctx, skipInfra)
		},
	}

	cmd.Flags().BoolVar(&skipInfra, "skip-infra", false, "Skip Terraform provisioning, run only reconcile")

	return cmd
}

func pruneNodesCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "prune-nodes",
		Short: "Delete NotReady K8s node objects not in the desired state",
		Long: `Remove stale Kubernetes node objects that are in NotReady status and do not
belong to and node defined in the current terraform.tfvars. This cleans up
ghost nodes left behind by previous scaling tests or interrupted operations.

Use --dry-run to preview which nodes would be deleted.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return a.RunPruneNodes(ctx)
		},
	}
}

func versionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the talops version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("talops " + version)
		},
	}
}

func downCmd(a *app.App) *cobra.Command {
	var (
		skipDrain bool
		force     bool
	)

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Drain nodes, shut down VMs, and destroy infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return a.RunDown(ctx, skipDrain, force)
		},
	}

	cmd.Flags().BoolVar(&skipDrain, "skip-drain", false, "Skip kubectl drain before destroying")
	cmd.Flags().BoolVar(&force, "force", false, "Force destroy (bypass safety checks)")

	return cmd
}
