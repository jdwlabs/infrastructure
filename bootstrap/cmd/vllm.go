package cmd

import (
	"errors"
	"fmt"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/vllm"
	"github.com/spf13/cobra"
)

// vllmDescription is the one-line identity the bare group prints, so a
// caller that lands here without context knows what this operates on.
const vllmDescription = "Compare and converge the GPU host's vLLM server to inference/vllm/serving.yaml, and audit the Hub for models that could replace it"

func vllmCmd(a *app.App) *cobra.Command {
	opts := &app.VLLMOptions{}

	cmd := &cobra.Command{
		Use:   "vllm",
		Short: "Inspect and converge the vLLM model server on the GPU host, and audit for replacement models",
		Long: `Read and converge the GPU host's vLLM Quadlet unit to inference/vllm/serving.yaml.

` + "`status`" + ` and ` + "`plan`" + ` read only. ` + "`apply`" + ` restarts the server through the
same validated, auto-rollback path hostconverge uses elsewhere: it stops
whatever is currently serving, starts the new server, and only then checks
that it answers serving.yaml's model — a failed check rolls back
automatically to whatever was serving before. It interrupts whatever
currently calls the server — ` + "`talops vllm plan`" + ` names them — for the length
of that restart and check.

` + "`audit`" + ` never contacts the GPU host: it reads the Hugging Face Hub for newly
released models that could replace the one serving.yaml serves and files
them to Jira (` + "`--dry-run`" + ` files nothing).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare invocation shows live state rather than a usage manual: the
		// caller can act on a drift report, but has to make a second call
		// after reading help text.
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			if !opts.JSON {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "bin: %s\ndescription: %s\n",
					executablePath(), vllmDescription)
			}
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunVLLMStatus(ctx, *opts)
		},
	}

	cmd.PersistentFlags().StringVar(&opts.Host, "host", "",
		"GPU VM address to target (default: gpu_vm_ip from tfvars)")
	cmd.PersistentFlags().StringVar(&opts.Spec, "spec", "",
		"Path to serving.yaml (default: inference/vllm/serving.yaml at the repo root; "+
			"a relative path resolves from the repo root, since talops chdirs there before every command)")
	cmd.PersistentFlags().BoolVar(&opts.JSON, "json", false,
		"Emit newline-delimited JSON, one object per state transition")

	cmd.AddCommand(
		vllmStatusCmd(a, opts),
		vllmPlanCmd(a, opts),
		vllmApplyCmd(a, opts),
		vllmAuditCmd(a, opts),
	)

	// Cobra reports an unrecognised flag on stderr, which a caller reading
	// stdout never sees. Reporting it as data — with the valid flags inline —
	// collapses the correction into one turn instead of a follow-up --help.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		_, _ = fmt.Fprintf(c.OutOrStdout(), "error: {code: unknown_flag, msg: %q}\nhelp[1]:\n  valid flags for `%s`: %s\n",
			err.Error(), c.CommandPath(), flagNames(c))
		return errQuiet{err}
	})

	return cmd
}

func vllmStatusCmd(a *app.App, opts *app.VLLMOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Compare serving.yaml, the applied record and the running container",
		Long: `One-shot comparison across serving.yaml (git), what was last applied, and what
the container is actually running. A read that fails reports "unknown" (the
applied record) rather than a clean result; a container that isn't running
reports "(not running)", not "unknown".

Exits non-zero when the layers disagree (drift: true), even when nothing
else failed: drift is what a caller acts on next.`,
		Example: `  talops vllm status
  talops vllm status --host 192.168.1.50`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunVLLMStatus(ctx, *opts)
		},
	}
}

func vllmPlanCmd(a *app.App, opts *app.VLLMOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Diff the rendered Quadlet unit against the one installed, without touching it",
		Long: `Render the Quadlet unit from serving.yaml and diff it against the file
installed on the GPU host, and list every consumer an apply would interrupt.
Nothing is written.

Drift is the answer, not a failure: this exits successfully either way, and
` + "`changed: true|false`" + ` is the signal to branch on.`,
		Example: `  talops vllm plan
  talops vllm plan --host 192.168.1.50`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunVLLMPlan(ctx, *opts)
		},
	}
}

func vllmApplyCmd(a *app.App, opts *app.VLLMOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Restart the server to match serving.yaml (health-gated, rolled back on failure)",
		Long: `Install the Quadlet unit rendered from serving.yaml: stop whatever server is
currently running, start the new one, and only then check that it answers
serving.yaml's model. A failed check rolls back automatically — to the
previous Quadlet unit on a later apply, or to whatever vllm.service's prior
enabled/active state was on a host's first apply. A no-op apply (host
already matches serving.yaml) runs one health check and restarts nothing.

Requires --confirm: this restarts the server and interrupts every consumer
for the length of the restart and health check. Also requires
inference/vllm/serving.yaml to be committed — an apply is recorded on the
host against the commit it came from.`,
		Example: `  talops vllm apply --confirm
  talops vllm apply --confirm --host 192.168.1.50`,
		Args: cobra.NoArgs,
		// Usage dumps and a second, differently-worded copy of the error on
		// stderr both compete with the structured report already on stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Out = cmd.OutOrStdout()
			ctx, cancel := signalContext()
			defer cancel()

			err := a.RunVLLMApply(ctx, *opts)
			var f *vllm.Failure
			if errors.As(err, &f) && f.Code == app.CodeConfirmRequired {
				return exitUsage{err: err}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false,
		"Confirm the restart (required; apply refuses without it)")
	return cmd
}
