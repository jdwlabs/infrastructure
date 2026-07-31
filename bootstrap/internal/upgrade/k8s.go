// Package upgrade wraps the Kubernetes upgrade path so its one mandatory
// safety flag is structural rather than remembered.
//
// talosctl upgrade-k8s re-applies cluster.extraManifests as its LAST stage and,
// since Talos 1.13, prunes inventory entries missing from the desired set by
// default. Two properties make that combination unusually unforgiving: the
// prune can delete objects a removed manifest created (a Namespace prune
// cascades to everything a GitOps release owns inside it), and because the
// manifest stage runs last, a failure there lands after the control plane and
// kubelets have already moved — there is no clean abort point.
//
// So the safe invocation is the default one here: --manifests-no-prune is
// injected unless pruning is opted into twice, and nothing mutates unless
// --apply is passed.
package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUsage marks an operator input problem. Callers map it to exit code 2 so an
// agent can tell a malformed request from a failed upgrade.
var ErrUsage = errors.New("usage")

// InventoryConfigMap is the ConfigMap Talos records applied bootstrap
// manifests in; its keys are what a prune operates on.
const InventoryConfigMap = "talos-bootstrap-manifests-inventory"

// manifestsNoPruneFlag is the flag whose absence is the hazard this package
// exists to remove.
const manifestsNoPruneFlag = "--manifests-no-prune"

// Options is a fully resolved upgrade request. Every field comes from an
// explicit flag: there is no ambient state that can change what runs.
type Options struct {
	To   string
	Node string

	// Apply turns the preview into a real upgrade. Zero value means dry run,
	// so an incomplete invocation degrades to the harmless one.
	Apply bool

	// AllowManifestPrune drops the injected --manifests-no-prune. It is
	// deliberately insufficient on its own; see ConfirmManifestPrune.
	AllowManifestPrune bool

	// ConfirmManifestPrune is the second, independent acknowledgement that
	// pruning is intended. Requiring two flags is what stops a single
	// copy-pasted flag from arming a cascading delete, and it replaces the
	// interactive confirmation that a CI or agent caller could not answer.
	ConfirmManifestPrune bool

	// Desired is the manifest inventory the upgrade would converge to. Keys
	// present in the cluster but missing here are what a prune would delete.
	Desired []string

	// Passthrough forwards unmanaged talosctl flags. Flags this package owns
	// are filtered out rather than forwarded, so passthrough cannot contradict
	// the guarantee above.
	Passthrough []string
}

// Result describes what was decided and what ran. It is the input to Report.
type Result struct {
	To               string
	Node             string
	Apply            bool
	DryRun           bool
	ManifestsNoPrune bool
	Executed         bool
	Args             []string
	PruneCandidates  []string

	// Inventory is every key currently recorded in the cluster.
	Inventory []string

	// DesiredSupplied records whether a desired set was given. Without one no
	// delta can be computed, and reporting zero candidates would assert a
	// check that never ran.
	DesiredSupplied bool

	// InventoryWarning is set when the inventory could not be read. It exists
	// so an unreadable inventory reports as unknown rather than as zero
	// candidates, which would make the prune guard look unnecessary.
	InventoryWarning string

	Output string
}

// Runner executes talosctl with the given args. Injected so the composition
// and refusal logic is testable without a cluster.
type Runner func(args []string) ([]byte, error)

// InventoryReader returns the manifest keys currently recorded in the cluster.
type InventoryReader func() ([]string, error)

// ownedFlags are the flags this package decides. A caller passing one of these
// through would be silently overriding a safety decision, so they are dropped
// from passthrough instead of forwarded.
var ownedFlags = map[string]bool{
	manifestsNoPruneFlag: true,
	"--dry-run":          true,
	"--to":               true,
}

// Validate rejects an inconsistent request before anything executes, so a
// refusal cannot have mutated the cluster.
func Validate(opts Options) error {
	if strings.TrimSpace(opts.To) == "" {
		return fmt.Errorf("%w: --to is required", ErrUsage)
	}
	if opts.AllowManifestPrune && !opts.ConfirmManifestPrune {
		return fmt.Errorf(
			"%w: --allow-manifest-prune removes the only guard against a cascading manifest delete, "+
				"so it also requires --confirm-manifest-prune. Nothing was run",
			ErrUsage,
		)
	}
	// A confirmation with nothing to confirm means the operator believes they
	// opted into something they did not; silently ignoring it would leave them
	// with a false model of what just ran.
	if opts.ConfirmManifestPrune && !opts.AllowManifestPrune {
		return fmt.Errorf(
			"%w: --confirm-manifest-prune only applies alongside --allow-manifest-prune. Nothing was run",
			ErrUsage,
		)
	}
	return nil
}

// BuildArgs composes the talosctl argv. The prune guard is added here rather
// than at the call site so every path — preview and real run alike — gets it.
func BuildArgs(opts Options) []string {
	var args []string
	if strings.TrimSpace(opts.Node) != "" {
		args = append(args, "-n", opts.Node)
	}
	args = append(args, "upgrade-k8s", "--to", opts.To)

	if !prunePermitted(opts) {
		args = append(args, manifestsNoPruneFlag)
	}
	if !opts.Apply {
		args = append(args, "--dry-run")
	}

	return append(args, filterPassthrough(opts.Passthrough)...)
}

// prunePermitted requires both acknowledgements. Validate rejects the
// half-armed combinations, so this only ever sees a coherent pair.
func prunePermitted(opts Options) bool {
	return opts.AllowManifestPrune && opts.ConfirmManifestPrune
}

// filterPassthrough drops flags this package owns, including their --flag=value
// forms, so a passthrough argument cannot re-enable pruning.
func filterPassthrough(args []string) []string {
	var kept []string
	for _, arg := range args {
		name := arg
		if i := strings.Index(arg, "="); i >= 0 {
			name = arg[:i]
		}
		if ownedFlags[name] {
			continue
		}
		kept = append(kept, arg)
	}
	return kept
}

// Run validates, reports the inventory delta, and executes. A nil inventory
// reader skips the delta rather than failing: the preview is still worth having
// without cluster access.
func Run(opts Options, runner Runner, inventory InventoryReader) (Result, error) {
	if err := Validate(opts); err != nil {
		return Result{}, err
	}

	res := Result{
		To:               opts.To,
		Node:             opts.Node,
		Apply:            opts.Apply,
		DryRun:           !opts.Apply,
		ManifestsNoPrune: !prunePermitted(opts),
		Args:             BuildArgs(opts),
		DesiredSupplied:  len(opts.Desired) > 0,
	}

	if inventory != nil {
		keys, err := inventory()
		switch {
		case err != nil:
			res.InventoryWarning = fmt.Sprintf("could not read %s: %v", InventoryConfigMap, err)
		default:
			res.Inventory = keys
			// Only compute a delta when there is something to diff against.
			if res.DesiredSupplied {
				res.PruneCandidates = InventoryDelta(keys, opts.Desired)
			}
		}
	}

	if runner == nil {
		return res, fmt.Errorf("no talosctl runner configured")
	}

	out, err := runner(res.Args)
	res.Output = string(out)
	res.Executed = true
	if err != nil {
		return res, fmt.Errorf("talosctl upgrade-k8s: %w", err)
	}
	return res, nil
}

// InventoryDelta returns the cluster keys absent from the desired set — exactly
// what a prune would delete — sorted so a report is stable between runs.
func InventoryDelta(cluster, desired []string) []string {
	want := make(map[string]bool, len(desired))
	for _, k := range desired {
		if k = strings.TrimSpace(k); k != "" {
			want[k] = true
		}
	}

	var delta []string
	for _, k := range cluster {
		if k = strings.TrimSpace(k); k != "" && !want[k] {
			delta = append(delta, k)
		}
	}
	sort.Strings(delta)
	return delta
}

// ParseInventoryKeys reads the keys from the inventory ConfigMap's data map.
// Unparseable output is an error rather than an empty key set, so a changed
// output shape cannot read as a converged inventory.
func ParseInventoryKeys(raw []byte) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}

	var data map[string]string
	if err := json.Unmarshal([]byte(trimmed), &data); err != nil {
		return nil, fmt.Errorf("parsing %s data: %w", InventoryConfigMap, err)
	}

	var keys []string
	for k := range data {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
