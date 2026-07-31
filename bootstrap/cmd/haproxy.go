package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// haproxyDescription is the one-line identity the bare group prints, so a
// caller that lands here without context knows what this operates on.
const haproxyDescription = "Inspect and converge the HAProxy load balancer fronting the Kubernetes API, Talos API, and ingress"

func haproxyCmd(a *app.App) *cobra.Command {
	opts := &app.HAProxyOptions{}

	cmd := &cobra.Command{
		Use:   "haproxy",
		Short: "Inspect and converge the HAProxy load balancer",
		Long: `Read and converge the load balancer's configuration.

This group never mutates Proxmox: the VM is Terraform-managed and human-applied,
so provisioning it stays behind ` + "`talops infra plan`" + ` and an operator's apply.
` + "`status`" + ` and ` + "`plan`" + ` read only. ` + "`apply`" + ` writes exactly one file — the generated
haproxy.cfg — through the same validated, auto-rollback path reconcile uses.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare invocation shows live state rather than a usage manual: the
		// caller can act on a health report, but has to make a second call
		// after reading help text.
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			if !opts.JSON {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "bin: %s\ndescription: %s\n",
					executablePath(), haproxyDescription)
			}
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunHAProxyStatus(ctx, *opts)
		},
	}

	cmd.PersistentFlags().StringVar(&opts.Host, "host", "",
		"Load-balancer address to target (default: haproxy_ip from tfvars)")
	cmd.PersistentFlags().StringSliceVar(&opts.Fields, "fields", nil,
		"Backend columns to print (default: name,addr,status,check)")
	cmd.PersistentFlags().BoolVar(&opts.Full, "full", false,
		"Print every backend column, and the untruncated diff")
	cmd.PersistentFlags().BoolVar(&opts.JSON, "json", false,
		"Emit newline-delimited JSON, one object per state transition")

	cmd.AddCommand(
		haproxyStatusCmd(a, opts),
		haproxyPlanCmd(a, opts),
		haproxyApplyCmd(a, opts),
	)

	// Cobra reports an unrecognised flag on stderr, which the caller reading
	// stdout never sees. Reporting it as data — with the valid flags inline —
	// collapses the correction into one turn instead of a follow-up --help.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		_, _ = fmt.Fprint(c.OutOrStdout(), haproxy.ReportError(
			&haproxy.Failure{Code: "unknown_flag", Msg: err.Error()},
			[]string{"valid flags for `" + c.CommandPath() + "`: " + flagNames(c)},
		))
		return errQuiet{err}
	})

	return cmd
}

func haproxyStatusCmd(a *app.App, opts *app.HAProxyOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report VM, SSH, service, config drift, and per-backend health",
		Long: `One-shot health of every layer between here and the control planes: the
Terraform-managed VM, SSH reachability, the haproxy service, whether the
deployed config still matches current cluster state, and each backend's health
as HAProxy itself reports it.

A layer that could not be read reports "unknown" rather than a clean result.`,
		Example: `  # Default schema: name, addr, status, check
  talops haproxy status

  # Verify a replacement on a temporary address before any cutover
  talops haproxy status --host 192.168.1.198

  # Extra columns, or every column the running HAProxy emits
  talops haproxy status --fields name,addr,status,check,weight,downtime
  talops haproxy status --full`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunHAProxyStatus(ctx, *opts)
		},
	}
}

func haproxyPlanCmd(a *app.App, opts *app.HAProxyOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Diff the rendered config against the one deployed, without touching it",
		Long: `Render the config from current cluster state and diff it against the file
installed on the load balancer. Nothing is written.

Drift is the answer, not a failure: this exits successfully either way, and
` + "`drift: true|false`" + ` is the signal to branch on.`,
		Example: `  talops haproxy plan
  talops haproxy plan --full          # untruncated diff
  talops haproxy plan --host 192.168.1.198`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunHAProxyPlan(ctx, *opts)
		},
	}
}

func haproxyApplyCmd(a *app.App, opts *app.HAProxyOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Push the rendered config (validated, rolled back on failure)",
		Long: `Install the config rendered from current cluster state, using the same path
reconcile uses: write, back up, validate with ` + "`haproxy -c`" + `, reload, and roll
back automatically if validation fails.

Idempotent — no drift means no push and ` + "`changed: false`" + `. This is a
config-only lever: it never provisions, destroys, or reconfigures the VM.
Combine with the global --dry-run to stop short of the push.`,
		Example: `  talops haproxy apply
  talops haproxy apply --dry-run      # report drift, push nothing
  talops haproxy apply --host 192.168.1.198`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunHAProxyApply(ctx, *opts)
		},
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// errQuiet marks an error whose message has already been written to stdout as
// structured output. Printing it a second time on stderr would leave the caller
// reconciling two differently-worded reports of one problem.
type errQuiet struct{ err error }

func (e errQuiet) Error() string { return e.err.Error() }
func (e errQuiet) Unwrap() error { return e.err }

func flagNames(cmd *cobra.Command) string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, "--"+f.Name)
	})
	return strings.Join(names, ", ")
}

// executablePath reports where this binary lives, with the home directory
// collapsed, so a caller can tell which build produced the output.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "talops"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return exe
	}
	if rel, err := filepath.Rel(home, exe); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return exe
}
