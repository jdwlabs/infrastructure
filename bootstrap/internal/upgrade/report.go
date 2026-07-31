package upgrade

import (
	"errors"
	"fmt"
	"strings"
)

// Report renders a Result as TOON for stdout. Progress and diagnostics belong
// on stderr: an agent reads this stream as data, so anything that is not data
// gets misread as some.
func Report(res Result) string {
	var b strings.Builder

	mode := "apply"
	if res.DryRun {
		mode = "dry-run"
	}

	b.WriteString("upgrade_k8s:\n")
	fmt.Fprintf(&b, "  mode: %s\n", mode)
	fmt.Fprintf(&b, "  to: %s\n", res.To)
	if res.Node != "" {
		fmt.Fprintf(&b, "  node: %s\n", res.Node)
	}
	fmt.Fprintf(&b, "  manifests_no_prune: %t\n", res.ManifestsNoPrune)
	fmt.Fprintf(&b, "  command: talosctl %s\n", strings.Join(res.Args, " "))

	// An unreadable inventory, an uncomputed delta, and a converged one are
	// three different answers. Collapsing any of them into "0" is what would
	// make the prune guard look unnecessary, so only a delta that was actually
	// computed ever prints a count.
	switch {
	case res.InventoryWarning != "":
		fmt.Fprintf(&b, "  inventory_warning: %s\n", res.InventoryWarning)
	case !res.DesiredSupplied:
		b.WriteString("  prune_candidates: unknown — desired set not supplied, so no delta was computed\n")
		writeKeyList(&b, "inventory", res.Inventory)
	case len(res.PruneCandidates) == 0:
		b.WriteString("  prune_candidates: 0 keys would be pruned\n")
	default:
		writeKeyList(&b, "prune_candidates", res.PruneCandidates)
	}

	if !res.ManifestsNoPrune {
		b.WriteString("  warning: pruning is enabled — every key above would be deleted, " +
			"and deleting a Namespace key cascades to everything inside it\n")
	}

	b.WriteString(helpFor(res))
	return b.String()
}

// writeKeyList renders a named TOON array with its count, so an agent never has
// to ask "how many are there".
func writeKeyList(b *strings.Builder, name string, keys []string) {
	if len(keys) == 0 {
		fmt.Fprintf(b, "  %s: 0\n", name)
		return
	}
	fmt.Fprintf(b, "  %s[%d]{key}:\n", name, len(keys))
	for _, k := range keys {
		fmt.Fprintf(b, "    %s\n", k)
	}
}

// helpFor offers the next step that follows from this result, not a fixed
// workflow: a preview suggests the real run, a completed run suggests the
// inventory re-check the runbooks depend on.
func helpFor(res Result) string {
	var hints []string

	switch {
	case !res.DesiredSupplied && res.InventoryWarning == "":
		hints = append(hints,
			"Compute the prune delta by passing the desired manifest keys: "+
				"`talops upgrade-k8s --to "+res.To+" --desired-from <file>`")
		if res.DryRun {
			hints = append(hints, "Run `talops upgrade-k8s --to "+res.To+" --apply` to perform the upgrade")
		}
	case res.DryRun && len(res.PruneCandidates) > 0:
		hints = append(hints,
			"Prune candidates exist: keep the default guard and run "+
				"`talops upgrade-k8s --to <version> --apply`")
		hints = append(hints,
			"Clear them first by converging cluster.extraManifests, then re-run this preview")
	case res.DryRun:
		hints = append(hints, "Run `talops upgrade-k8s --to "+res.To+" --apply` to perform the upgrade")
	default:
		hints = append(hints,
			"Re-check the inventory: `kubectl -n kube-system get cm "+
				InventoryConfigMap+" -o jsonpath='{.data}'`")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "help[%d]:\n", len(hints))
	for _, h := range hints {
		fmt.Fprintf(&b, "  %s\n", h)
	}
	return b.String()
}

// ReportError renders an error as TOON on stdout, pairing it with the command
// that fixes it so the agent self-corrects in one turn instead of following up
// with --help.
func ReportError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "usage: ")

	var b strings.Builder
	fmt.Fprintf(&b, "error: %s\n", msg)

	if errors.Is(err, ErrUsage) {
		b.WriteString("help:\n")
		switch {
		case strings.Contains(msg, "--confirm-manifest-prune only applies"):
			b.WriteString("  Drop --confirm-manifest-prune, or add --allow-manifest-prune if pruning is intended\n")
		case strings.Contains(msg, "--confirm-manifest-prune"):
			b.WriteString("  talops upgrade-k8s --to <version> --apply " +
				"--allow-manifest-prune --confirm-manifest-prune\n")
			b.WriteString("  Preview what pruning would delete first: " +
				"talops upgrade-k8s --to <version>\n")
		case strings.Contains(msg, "--to is required"):
			b.WriteString("  talops upgrade-k8s --to <version>\n")
		}
		b.WriteString("  Valid flags: --to, --node, --apply, " +
			"--allow-manifest-prune, --confirm-manifest-prune, --desired-from\n")
	}

	return b.String()
}
