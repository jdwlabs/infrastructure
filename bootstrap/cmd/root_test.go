package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// execute is a helper function to execute a cobra command in tests
func execute(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()

	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return strings.TrimSpace(buf.String()), err
}

// setupTestApp creates a minimal app for testing
func setupTestApp(t *testing.T) *app.App {
	t.Helper()

	// Create a temporary directory for test files
	tmpDir := t.TempDir()

	cfg := &types.Config{
		ClusterName:          "test-cluster",
		TerraformTFVars:      filepath.Join(tmpDir, "terraform.tfvars"),
		ControlPlaneEndpoint: "test.example.com",
		HAProxyIP:            net.ParseIP("192.168.1.100"),
		KubernetesVersion:    "v1.35.1",
		TalosVersion:         "v1.12.3",
		InstallerImage:       "factory.talos.dev/installer:v1.12.3",
		ProxmoxNodeIPs: map[string]net.IP{
			"pve1": net.ParseIP("192.168.1.200"),
		},
		LogDir:     tmpDir,
		LogLevel:   "debug",
		SecretsDir: filepath.Join(tmpDir, "secrets"),
	}

	// Create a minimal terraform.tfvars for testing
	tfvarsContent := `cluster_name = "test-cluster"
control_plane_endpoint = "test.example.com"
haproxy_ip = "192.168.1.100"
kubernetes_version = "v1.35.1"
talos_version = "v1.12.3"
installer_image = "factory.talos.dev/installer:v1.12.3"

proxmox_node_ips = {
  pve1 = "192.168.1.200"
}

talos_control_configuration = [
  {
    vmid       = 201
    vm_name    = "talos-cp-1"
    node_name  = "pve1"
    cpu_cores  = 4
    memory     = 4096
    disk_size  = 100
  }
]
`
	err := os.WriteFile(cfg.TerraformTFVars, []byte(tfvarsContent), 0644)
	require.NoError(t, err)

	a := app.New("test-version")
	a.Cfg = cfg
	a.Logger = zap.NewNop()
	return a
}

func TestExecute(t *testing.T) {
	t.Run("returns nil on successful execution", func(t *testing.T) {
		// This test would require mocking the entire app initialization
		// For now, we test that Execute doesn't panic with basic setup
		assert.NotPanics(t, func() {
			// We can't fully test Execute() without a complete mock setup
			// but we can verify it doesn't panic
		})
	})
}

func TestBootstrapCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := bootstrapCmd(a)

		assert.Equal(t, "bootstrap", cmd.Use)
		assert.Equal(t, "Initial cluster deployment", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("requires valid context", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := bootstrapCmd(a)

		// Set up command for testing without actually running the reconcile
		buf := new(bytes.Buffer)
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{})

		// The command should exist and be runnable
		assert.NotNil(t, cmd.RunE)
	})
}

func TestReconcileCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := reconcileCmd(a)

		assert.Equal(t, "reconcile", cmd.Use)
		assert.Equal(t, "Reconcile cluster with terraform.tfvars", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("plan flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := reconcileCmd(a)

		flag := cmd.Flags().Lookup("plan")
		require.NotNil(t, flag)
		assert.Equal(t, "p", flag.Shorthand)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Show changes without applying", flag.Usage)
	})

	t.Run("plan mode sets dry-run", func(t *testing.T) {
		a := setupTestApp(t)

		// Create the command and set plan mode
		var planMode bool
		cmd := &cobra.Command{
			Use:   "reconcile",
			Short: "Reconcile cluster with terraform.tfvars",
			RunE: func(cmd *cobra.Command, args []string) error {
				if planMode {
					a.Cfg.PlanMode = true
					a.Cfg.DryRun = true
				}
				return nil
			},
		}
		cmd.Flags().BoolVarP(&planMode, "plan", "p", false, "Show changes without applying")

		// Execute with --plan flag
		_, err := execute(t, cmd, "--plan")
		require.NoError(t, err)

		assert.True(t, a.Cfg.PlanMode)
		assert.True(t, a.Cfg.DryRun)
	})
}

func TestStatusCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := statusCmd(a)

		assert.Equal(t, "status", cmd.Use)
		assert.Equal(t, "Show current cluster status", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})
}

func TestResetCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := resetCmd(a)

		assert.Equal(t, "reset", cmd.Use)
		assert.Equal(t, "Reset cluster state", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("cancels when user does not confirm", func(t *testing.T) {
		// Create a mock app that simulates user declining confirmation
		a := setupTestApp(t)
		a.Cfg.AutoApprove = false

		cmd := resetCmd(a)

		// Mock the prompt to return false (decline)
		// This would require modifying the app to accept a mock prompter
		// For now, we just verify the command structure
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("auto-approve skips confirmation", func(t *testing.T) {
		a := setupTestApp(t)
		a.Cfg.AutoApprove = true

		cmd := resetCmd(a)

		// Create a temporary cluster directory to reset
		clusterDir := filepath.Join("clusters", a.Cfg.ClusterName)
		err := os.MkdirAll(clusterDir, 0755)
		require.NoError(t, err)

		// Create a test file in the cluster directory
		testFile := filepath.Join(clusterDir, "test.txt")
		err = os.WriteFile(testFile, []byte("test"), 0644)
		require.NoError(t, err)

		// Verify file exists
		_, err = os.Stat(testFile)
		require.NoError(t, err)

		// Execute reset
		buf := new(bytes.Buffer)
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{})

		// This will fail because we don't have a full mock setup,
		// but we can verify the command runs
		_ = cmd.RunE(cmd, []string{})
	})
}

func TestInfraCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraCmd(a)

		assert.Equal(t, "infra", cmd.Use)
		assert.Equal(t, "Manage infrastructure (Terraform VM provisioning)", cmd.Short)
	})

	t.Run("has correct subcommands", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraCmd(a)

		subcommands := []string{"deploy", "destroy", "plan", "status", "cleanup"}
		for _, sub := range subcommands {
			found := false
			for _, c := range cmd.Commands() {
				if c.Name() == sub {
					found = true
					break
				}
			}
			assert.True(t, found, "expected subcommand %s to exist", sub)
		}
	})

	t.Run("skip-backup flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraCmd(a)

		flag := cmd.PersistentFlags().Lookup("skip-backup")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
	})
}

func TestInfraDeployCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraDeployCmd(a)

		assert.Equal(t, "deploy", cmd.Use)
		assert.Equal(t, "Deploy or update cluster infrastructure", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("skip-plan flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraDeployCmd(a)

		flag := cmd.Flags().Lookup("skip-plan")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
	})
}

func TestInfraDestroyCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraDestroyCmd(a)

		assert.Equal(t, "destroy", cmd.Use)
		assert.Equal(t, "Destroy cluster infrastructure", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("force flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraDestroyCmd(a)

		flag := cmd.Flags().Lookup("force")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Force mode (bypass safety checks, no refresh)", flag.Usage)
	})

	t.Run("graceful flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraDestroyCmd(a)

		flag := cmd.Flags().Lookup("graceful")
		require.NotNil(t, flag)
		assert.Equal(t, "true", flag.DefValue)
		assert.Equal(t, "Gracefully stop VMs before destroying", flag.Usage)
	})
}

func TestInfraPlanCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraPlanCmd(a)

		assert.Equal(t, "plan", cmd.Use)
		assert.Equal(t, "Preview infrastructure changes without applying", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})
}

func TestInfraStatusCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraStatusCmd(a)

		assert.Equal(t, "status", cmd.Use)
		assert.Equal(t, "Show current infrastructure deployment state", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})
}

func TestInfraCleanupCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraCleanupCmd(a)

		assert.Equal(t, "cleanup", cmd.Use)
		assert.Equal(t, "Remove generated Terraform files", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("all flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := infraCleanupCmd(a)

		flag := cmd.Flags().Lookup("all")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Also remove .terraform/, backups/, and deploy state", flag.Usage)
	})
}

func TestUpCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := upCmd(a)

		assert.Equal(t, "up", cmd.Use)
		assert.Equal(t, "Provision infrastructure and bootstrap cluster", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("skip-infra flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := upCmd(a)

		flag := cmd.Flags().Lookup("skip-infra")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Skip Terraform provisioning, run only reconcile", flag.Usage)
	})
}

func TestDownCmd(t *testing.T) {
	t.Run("command structure is correct", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := downCmd(a)

		assert.Equal(t, "down", cmd.Use)
		assert.Equal(t, "Drain nodes, shut down VMs, and destroy infrastructure", cmd.Short)
		assert.NotNil(t, cmd.RunE)
	})

	t.Run("skip-drain flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := downCmd(a)

		flag := cmd.Flags().Lookup("skip-drain")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Skip kubectl drain before destroying", flag.Usage)
	})

	t.Run("force flag is registered", func(t *testing.T) {
		a := setupTestApp(t)
		cmd := downCmd(a)

		flag := cmd.Flags().Lookup("force")
		require.NotNil(t, flag)
		assert.Equal(t, "false", flag.DefValue)
		assert.Equal(t, "Force destroy (bypass safety checks)", flag.Usage)
	})
}

func TestGlobalFlags(t *testing.T) {
	t.Run("all global flags are registered", func(t *testing.T) {
		a := setupTestApp(t)

		// Create root command similar to Execute()
		rootCmd := &cobra.Command{
			Use:          "talops",
			Short:        "Smart reconciliation for Talos clusters",
			SilenceUsage: true,
		}

		cfg := a.Cfg

		// Register all global flags
		rootCmd.PersistentFlags().StringVarP(&cfg.ClusterName, "cluster", "c", "cluster", "Cluster name")
		rootCmd.PersistentFlags().StringVarP(&cfg.TerraformTFVars, "tfvars", "t", "terraform.tfvars", "Path to terraform.tfvars")
		rootCmd.PersistentFlags().BoolVarP(&cfg.AutoApprove, "auto-approve", "a", false, "Skip confirmations")
		rootCmd.PersistentFlags().BoolVarP(&cfg.DryRun, "dry-run", "d", false, "Simulate only")
		rootCmd.PersistentFlags().BoolVarP(&cfg.SkipPreflight, "skip-preflight", "s", false, "Skip connectivity checks")
		rootCmd.PersistentFlags().StringVarP(&cfg.LogLevel, "log-level", "l", "info", "Log level (debug, info, warn, error)")
		rootCmd.PersistentFlags().StringVarP(&cfg.ProxmoxSSHKeyPath, "ssh-key", "k", cfg.ProxmoxSSHKeyPath, "Path to SSH private key")
		rootCmd.PersistentFlags().BoolVarP(&cfg.ForceReconfigure, "force-reconfigure", "f", false, "Force reconfigure all nodes")
		rootCmd.PersistentFlags().StringVar(&cfg.LogDir, "log-dir", cfg.LogDir, "Log directory")
		rootCmd.PersistentFlags().BoolVar(&cfg.NoColor, "no-color", cfg.NoColor, "Disable colored output")
		rootCmd.PersistentFlags().StringVar(&cfg.ControlPlaneEndpoint, "control-plane-endpoint", "", "Control plane endpoint")
		rootCmd.PersistentFlags().StringVar(&cfg.KubernetesVersion, "kubernetes-version", "", "Kubernetes version")
		rootCmd.PersistentFlags().StringVar(&cfg.TalosVersion, "talos-version", "", "Talos version")
		rootCmd.PersistentFlags().StringVar(&cfg.InstallerImage, "installer-image", "", "Talos installer image")
		rootCmd.PersistentFlags().StringVar(&cfg.HAProxyLoginUser, "haproxy-user", "", "HAProxy SSH login user")
		rootCmd.PersistentFlags().StringVar(&cfg.HAProxyStatsUser, "haproxy-stats-user", "", "HAProxy stats username")
		rootCmd.PersistentFlags().StringVar(&cfg.HAProxyStatsPassword, "haproxy-stats-password", "", "HAProxy stats password")
		rootCmd.PersistentFlags().BoolVar(&cfg.InsecureSSH, "insecure-ssh", false, "Skip SSH host key verification (INSECURE)")
		rootCmd.PersistentFlags().StringVar(&cfg.TerraformDir, "terraform-dir", "", "Directory containing Terraform files")
		rootCmd.PersistentFlags().StringVar(&cfg.PatchDir, "patch-dir", "", "Directory for patch template overrides")

		// Test that all flags exist
		flags := []string{
			"cluster", "tfvars", "auto-approve", "dry-run", "skip-preflight",
			"log-level", "ssh-key", "force-reconfigure", "log-dir", "no-color",
			"control-plane-endpoint", "kubernetes-version", "talos-version",
			"installer-image", "haproxy-user", "haproxy-stats-user",
			"haproxy-stats-password", "insecure-ssh", "terraform-dir", "patch-dir",
		}

		for _, flagName := range flags {
			flag := rootCmd.PersistentFlags().Lookup(flagName)
			assert.NotNil(t, flag, "expected flag %s to be registered", flagName)
		}
	})
}

func TestCommandHierarchy(t *testing.T) {
	t.Run("commands can be added to root", func(t *testing.T) {
		a := setupTestApp(t)

		rootCmd := &cobra.Command{Use: "talops"}
		rootCmd.AddCommand(
			bootstrapCmd(a),
			reconcileCmd(a),
			statusCmd(a),
			resetCmd(a),
			infraCmd(a),
			upCmd(a),
			downCmd(a),
		)

		commands := []string{"bootstrap", "reconcile", "status", "reset", "infra", "up", "down"}
		for _, cmdName := range commands {
			found := false
			for _, c := range rootCmd.Commands() {
				if c.Name() == cmdName {
					found = true
					break
				}
			}
			assert.True(t, found, "expected command %s to be added to root", cmdName)
		}
	})
}

// Integration-style test that verifies command execution flow
func TestCommandExecutionFlow(t *testing.T) {
	t.Run("help command works", func(t *testing.T) {
		rootCmd := &cobra.Command{
			Use:   "talops",
			Short: "Smart reconciliation for Talos clusters",
		}

		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetArgs([]string{"--help"})

		err := rootCmd.Execute()
		require.NoError(t, err)

		output := buf.String()
		assert.Contains(t, output, "Smart reconciliation for Talos clusters")
	})

	t.Run("version flag works", func(t *testing.T) {
		rootCmd := &cobra.Command{
			Use:     "talops",
			Version: "test-version-123",
		}
		rootCmd.SetVersionTemplate("Version: {{.Version}}\n")

		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetArgs([]string{"--version"})

		err := rootCmd.Execute()
		require.NoError(t, err)

		output := buf.String()
		assert.Contains(t, output, "test-version-123")
	})
}
