package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/kubectl"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/upgrade"
	"github.com/spf13/cobra"
)

// exitUsage is returned for operator input errors so an agent can distinguish a
// malformed request from a failed upgrade.
type exitUsage struct{ err error }

func (e exitUsage) Error() string { return e.err.Error() }
func (e exitUsage) Unwrap() error { return e.err }

// Indirection so tests can substitute the live dependencies. Both of these
// reach a real cluster, and upgrade-k8s is the most destructive command talops
// issues, so no test may be able to invoke them by accident.
var (
	talosctlRunnerFor  = func(a *app.App) upgrade.Runner { return a.TalosctlRunner() }
	inventoryReaderFor = inventoryReader
)

func upgradeK8sCmd(a *app.App) *cobra.Command {
	var (
		to          string
		node        string
		apply       bool
		allowPrune  bool
		confirm     bool
		desiredFrom string
	)

	cmd := &cobra.Command{
		Use:   "upgrade-k8s",
		Short: "Upgrade Kubernetes with the manifest-prune guard applied by default",
		Long: `Upgrade the Kubernetes version via talosctl with --manifests-no-prune applied
by default.

Talos records every applied bootstrap manifest in the
kube-system/talos-bootstrap-manifests-inventory ConfigMap and prunes entries
missing from the desired set. That sync is the LAST stage of the upgrade, so a
prune that deletes something still in use lands after the control plane and
kubelets have already moved - there is no clean abort point. Pruning a
Namespace key cascades to every object inside it.

This command therefore previews by default and never prunes unless pruning is
opted into twice. It reads the inventory before the run and reports the keys a
prune would delete.`,
		Example: `  # Preview (default): nothing is mutated
  talops upgrade-k8s --to 1.36.3 --node 192.168.1.50

  # Perform the upgrade, prune guard still applied
  talops upgrade-k8s --to 1.36.3 --node 192.168.1.50 --apply

  # Report which inventory keys a prune would delete
  talops upgrade-k8s --to 1.36.3 --desired-from desired-keys.txt

  # Allow pruning - requires both flags, and nothing runs without the second
  talops upgrade-k8s --to 1.36.3 --apply --allow-manifest-prune --confirm-manifest-prune`,
		// Reject unknown positional arguments rather than ignoring them: a
		// dropped argument would return plausible output for a request that was
		// not the one made.
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			// The global --dry-run predates this command and means the same
			// thing, so honouring it is expected. Combined with --apply it is a
			// contradiction, and picking a winner silently would leave the
			// operator unsure whether the cluster moved.
			if a.Cfg.DryRun && apply {
				return emitUsage(cmd, fmt.Errorf(
					"%w: --apply and --dry-run contradict each other. Drop --dry-run to upgrade, "+
						"or drop --apply to preview. Nothing was run", upgrade.ErrUsage))
			}

			opts := upgrade.Options{
				To:                   to,
				Node:                 node,
				Apply:                apply,
				AllowManifestPrune:   allowPrune,
				ConfirmManifestPrune: confirm,
			}

			// Validated up front so a refusal cannot have run anything. This
			// comes first so the primary required flag is what gets reported.
			if err := upgrade.Validate(opts); err != nil {
				return emitUsage(cmd, err)
			}

			// talosctl refuses upgrade-k8s across multiple nodes, and with no
			// -n it targets every node in the talosconfig. Requiring the node
			// here turns that raw downstream error into an actionable one and
			// guarantees a single, named target.
			if strings.TrimSpace(node) == "" {
				return emitUsage(cmd, fmt.Errorf(
					"%w: --node is required — talosctl runs upgrade-k8s against exactly one "+
						"control plane node, and targets every node in the talosconfig without it",
					upgrade.ErrUsage))
			}

			desired, err := readDesiredKeys(desiredFrom)
			if err != nil {
				return emitUsage(cmd, err)
			}
			opts.Desired = desired

			res, runErr := upgrade.Run(opts, talosctlRunnerFor(a), inventoryReaderFor(ctx, a))

			// The report is printed even on failure: when the manifest stage
			// fails the operator most needs to know which keys were at risk.
			_, _ = fmt.Fprint(cmd.OutOrStdout(), upgrade.Report(res))
			if runErr != nil {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), upgrade.ReportError(runErr))
				return runErr
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "Target Kubernetes version (required, e.g. 1.36.3)")
	cmd.Flags().StringVar(&node, "node", "", "Control plane node to run the upgrade against (required)")
	cmd.Flags().BoolVar(&apply, "apply", false, "Perform the upgrade (default is a preview that mutates nothing)")
	cmd.Flags().BoolVar(&allowPrune, "allow-manifest-prune", false,
		"Drop --manifests-no-prune and allow the manifest sync to prune (also requires --confirm-manifest-prune)")
	cmd.Flags().BoolVar(&confirm, "confirm-manifest-prune", false,
		"Second, explicit acknowledgement that pruning inventory keys is intended")
	cmd.Flags().StringVar(&desiredFrom, "desired-from", "",
		"File of desired inventory keys (one per line) to diff the cluster inventory against")

	return cmd
}

// emitUsage prints the structured error on stdout and marks it as a usage
// problem so Execute can map it to exit code 2.
func emitUsage(cmd *cobra.Command, err error) error {
	_, _ = fmt.Fprint(cmd.OutOrStdout(), upgrade.ReportError(err))
	return exitUsage{err: err}
}

// readDesiredKeys loads the desired inventory keys, skipping blanks and
// comments. A path that was given but cannot be read is an error rather than an
// empty set, so a typo cannot silently become "no keys are desired".
func readDesiredKeys(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: --desired-from %s could not be read: %v", upgrade.ErrUsage, path, err)
	}

	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: --desired-from %s contained no keys", upgrade.ErrUsage, path)
	}
	return keys, nil
}

// inventoryReader reads the manifest inventory read-only. Failure is reported as
// a warning by the caller rather than aborting: a preview is still worth having
// without cluster access, and the prune guard does not depend on this read.
func inventoryReader(ctx context.Context, a *app.App) upgrade.InventoryReader {
	return func() ([]string, error) {
		client := kubectl.NewClient(a.Logger)
		if a.Session != nil && a.Session.AuditLog != nil {
			client.SetAuditLogger(a.Session.AuditLog)
		}
		raw, err := client.GetConfigMapData(ctx, "kube-system", upgrade.InventoryConfigMap)
		if err != nil {
			return nil, err
		}
		return upgrade.ParseInventoryKeys(raw)
	}
}

// ExitCode maps an error to a process exit status: 2 for an operator input
// problem, 1 for a genuine failure. Separating them lets a caller retry a
// malformed request without treating it as a failed cluster operation.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var u exitUsage
	if errors.As(err, &u) || errors.Is(err, upgrade.ErrUsage) {
		return 2
	}
	return 1
}
