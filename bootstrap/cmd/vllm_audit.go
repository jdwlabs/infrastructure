package cmd

import (
	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/spf13/cobra"
)

// auditEndpointsFor lets tests point the audit at fakes; the zero value is
// the real Hub and GitHub.
var auditEndpointsFor = func() app.AuditEndpoints { return app.AuditEndpoints{} }

func vllmAuditCmd(a *app.App, opts *app.VLLMOptions) *cobra.Command {
	var jsonOut bool
	var ignoredHost string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "List newly released models that could replace the one serving.yaml serves",
		Long: `Read the Hugging Face Hub for models created in the last week by the
organisations in inference/vllm/audit.yaml (plus trending repos, flagged
unvetted), keep those vLLM can serve with a tool parser and that fit the GPU,
and report them with a starting set of trial steps for each.

Files the list as one Jira ticket per ISO week (JIRA_BASE_URL, JIRA_EMAIL,
JIRA_API_TOKEN), or a comment when nothing is new. --dry-run performs every
read, Jira's included when those are set, and writes nothing. HF_TOKEN, when
set, lets gated repos be checked.

It never contacts the GPU host, never decrypts the vault, and never changes
serving.yaml.`,
		Example: `  talops vllm audit --dry-run
  talops vllm audit --json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Only the nearest PersistentPreRunE runs, so this replaces the root
		// hook: the audit reads public APIs and has no use for the vault
		// hydrate, the stale-vault git check or the cluster scaffold, and on a
		// runner with sops but no age key the hydrate alone would fail it.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.AnchorToRepoRoot(cmd); err != nil {
				return err
			}
			if err := a.InitConfig(cmd); err != nil {
				return err
			}
			return a.InitSession(cmd, app.SkipPrerequisites())
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return a.RunVLLMAudit(ctx, app.AuditOptions{
				Spec:      opts.Spec,
				JSON:      jsonOut,
				DryRun:    a.Cfg.DryRun,
				Out:       cmd.OutOrStdout(),
				Endpoints: auditEndpointsFor(),
			})
		},
	}
	// Local flags shadow the group's persistent ones, which is the only way
	// to give them audit-specific help: the audit prints one JSON object, not
	// one per state transition, and it has no host to target.
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the report as one JSON object")
	cmd.Flags().StringVar(&ignoredHost, "host", "", "Ignored: the audit never contacts the GPU host")
	return cmd
}
