package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/secrets"
	"github.com/spf13/cobra"
)

func secretsCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the encrypted SOPS vault shared via git",
		Long: `Manage the encrypted secret vault.

The Talos secrets bundle, talosconfig, bootstrap state, and terraform.tfvars are
stored as SOPS+age encrypted *.enc.yaml files committed to git. Plaintext working
copies are decrypted on demand and stay gitignored.`,
	}
	cmd.AddCommand(
		secretsStatusCmd(a),
		secretsHydrateCmd(a),
		secretsSealCmd(a),
		secretsLockCmd(a),
		secretsEditCmd(a),
		secretsAddDeviceCmd(a),
	)
	return cmd
}

func secretsStatusCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show vault recipients and per-artifact state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Vault.Available(); err != nil {
				return err
			}
			recips, err := secrets.Recipients()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not read recipients: %v\n", err)
			}
			fmt.Printf("Recipients (%d):\n", len(recips))
			for _, r := range recips {
				fmt.Printf("  - %s\n", r)
			}
			fmt.Println("\nArtifacts:")
			fmt.Printf("  %-28s %-9s %s\n", "ENCRYPTED (committed)", "PLAINTEXT", "")
			for _, st := range a.Vault.Status() {
				enc := "missing"
				if st.HasEnc {
					enc = "present"
				}
				plain := "absent"
				if st.HasPlain {
					plain = "present"
				}
				fmt.Printf("  %-28s %-9s %s\n", enc+" ("+st.Enc+")", plain, st.Plain)
			}
			return nil
		},
	}
}

func secretsHydrateCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "hydrate",
		Short: "Decrypt the vault into plaintext working files",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Vault.Available(); err != nil {
				return err
			}
			if err := a.Vault.KeyAvailable(); err != nil {
				return err
			}
			written, err := a.Vault.Hydrate(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("hydrated %d file(s)\n", len(written))
			return nil
		},
	}
}

func secretsSealCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "seal",
		Short: "Encrypt changed plaintext working files into the vault",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Vault.Available(); err != nil {
				return err
			}
			sealed, err := a.Vault.Seal(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("sealed %d file(s) — commit the .enc.yaml changes to share them\n", len(sealed))
			return nil
		},
	}
}

func secretsLockCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Seal the vault and remove plaintext working copies",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Vault.Available(); err != nil {
				return err
			}
			if err := a.Vault.Lock(context.Background()); err != nil {
				return err
			}
			fmt.Println("vault locked: plaintext working copies removed")
			return nil
		},
	}
}

func secretsEditCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <tfvars|secrets|talosconfig|state>",
		Short: "Edit a secret in $EDITOR and re-seal it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Vault.Available(); err != nil {
				return err
			}
			entry, ok := a.Vault.EntryByName(args[0])
			if !ok {
				return fmt.Errorf("unknown secret %q (expected: tfvars, secrets, talosconfig, state)", args[0])
			}
			// Ensure a plaintext copy exists to edit.
			if _, err := os.Stat(entry.Plain); err != nil {
				if err := a.Vault.KeyAvailable(); err != nil {
					return err
				}
				if _, err := a.Vault.Hydrate(context.Background()); err != nil {
					return err
				}
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "nano"
			}
			ed := exec.Command(editor, entry.Plain) //nolint:gosec // editor is operator-controlled
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := ed.Run(); err != nil {
				return fmt.Errorf("editor: %w", err)
			}
			sealed, err := a.Vault.Seal(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("sealed %d file(s) — commit the .enc.yaml changes\n", len(sealed))
			return nil
		},
	}
}

func secretsAddDeviceCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "add-device <age-public-key>",
		Short: "Grant a device's age key access and re-key the vault",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := a.Vault.Available(); err != nil {
				return err
			}
			if err := a.Vault.KeyAvailable(); err != nil {
				return err
			}
			if err := a.Vault.AddRecipient(ctx, strings.TrimSpace(args[0])); err != nil {
				return err
			}
			fmt.Println("device added — commit .sops.yaml and the re-keyed .enc.yaml files")
			return nil
		},
	}
}
