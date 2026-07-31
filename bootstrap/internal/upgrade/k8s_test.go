package upgrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		wantArgs []string
	}{
		{
			name: "default injects manifests-no-prune and dry-run",
			opts: Options{To: "1.36.3", Node: "192.168.1.50"},
			wantArgs: []string{
				"-n", "192.168.1.50", "upgrade-k8s",
				"--to", "1.36.3", "--manifests-no-prune", "--dry-run",
			},
		},
		{
			// The safe flag must survive the transition from preview to real
			// run: a plan whose argv differs from the apply argv proves nothing.
			name: "apply keeps manifests-no-prune and drops dry-run",
			opts: Options{To: "1.36.3", Node: "192.168.1.50", Apply: true},
			wantArgs: []string{
				"-n", "192.168.1.50", "upgrade-k8s",
				"--to", "1.36.3", "--manifests-no-prune",
			},
		},
		{
			name: "confirmed prune opt-in omits manifests-no-prune",
			opts: Options{
				To: "1.36.3", Node: "192.168.1.50", Apply: true,
				AllowManifestPrune:   true,
				ConfirmManifestPrune: true,
			},
			wantArgs: []string{
				"-n", "192.168.1.50", "upgrade-k8s", "--to", "1.36.3",
			},
		},
		{
			name: "prune opt-in still previews under dry-run",
			opts: Options{
				To: "1.36.3", Node: "192.168.1.50",
				AllowManifestPrune:   true,
				ConfirmManifestPrune: true,
			},
			wantArgs: []string{
				"-n", "192.168.1.50", "upgrade-k8s", "--to", "1.36.3", "--dry-run",
			},
		},
		{
			name: "no node omits the -n selector",
			opts: Options{To: "1.36.3"},
			wantArgs: []string{
				"upgrade-k8s", "--to", "1.36.3", "--manifests-no-prune", "--dry-run",
			},
		},
		{
			name: "passthrough args land after the managed flags",
			opts: Options{
				To: "1.36.3", Node: "192.168.1.50",
				Passthrough: []string{"--endpoint", "10.0.0.1"},
			},
			wantArgs: []string{
				"-n", "192.168.1.50", "upgrade-k8s", "--to", "1.36.3",
				"--manifests-no-prune", "--dry-run", "--endpoint", "10.0.0.1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantArgs, BuildArgs(tt.opts))
		})
	}
}

// A passthrough arg must never be able to reintroduce pruning by smuggling the
// negated form past the flag composition, which would defeat the whole wrapper.
func TestBuildArgsPassthroughCannotDisablePruneGuard(t *testing.T) {
	args := BuildArgs(Options{
		To: "1.36.3", Apply: true,
		Passthrough: []string{"--manifests-no-prune=false"},
	})
	assert.NotContains(t, args, "--manifests-no-prune=false")
	assert.Contains(t, args, "--manifests-no-prune")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		opts        Options
		wantErr     error
		wantMessage string
	}{
		{
			name: "dry-run default is valid with only --to",
			opts: Options{To: "1.36.3"},
		},
		{
			name: "apply with the safe default is valid",
			opts: Options{To: "1.36.3", Apply: true},
		},
		{
			name:        "missing --to is a usage error",
			opts:        Options{},
			wantErr:     ErrUsage,
			wantMessage: "--to is required",
		},
		{
			// The whole point of the ticket: opting into prune must not be a
			// single-flag decision.
			name:        "prune opt-in without confirmation refuses",
			opts:        Options{To: "1.36.3", Apply: true, AllowManifestPrune: true},
			wantErr:     ErrUsage,
			wantMessage: "--confirm-manifest-prune",
		},
		{
			name:        "prune opt-in without confirmation refuses under dry-run too",
			opts:        Options{To: "1.36.3", AllowManifestPrune: true},
			wantErr:     ErrUsage,
			wantMessage: "--confirm-manifest-prune",
		},
		{
			// Fail loud rather than silently ignoring a flag that cannot apply:
			// a confirmation with nothing to confirm means the operator thinks
			// they opted into something they did not.
			name:        "confirmation without opt-in refuses",
			opts:        Options{To: "1.36.3", Apply: true, ConfirmManifestPrune: true},
			wantErr:     ErrUsage,
			wantMessage: "--allow-manifest-prune",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.opts)
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Contains(t, err.Error(), tt.wantMessage)
		})
	}
}

// Validate must reject before anything is executed, so the refusal path cannot
// mutate the cluster even in principle.
func TestValidateRefusalIsCheckedBeforeExecution(t *testing.T) {
	var called bool
	runner := func(_ []string) ([]byte, error) {
		called = true
		return nil, nil
	}

	res, err := Run(Options{To: "1.36.3", Apply: true, AllowManifestPrune: true}, runner, nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage)
	assert.False(t, called, "talosctl must not be invoked on the refusal path")
	assert.False(t, res.Executed)
}

func TestRunDryRunIsTheDefault(t *testing.T) {
	var gotArgs []string
	runner := func(args []string) ([]byte, error) {
		gotArgs = args
		return []byte("updating manifests (dry run)"), nil
	}

	res, err := Run(Options{To: "1.36.3", Node: "192.168.1.50"}, runner, nil)

	require.NoError(t, err)
	assert.True(t, res.DryRun, "an invocation with no --apply must be a dry run")
	assert.True(t, res.ManifestsNoPrune, "the safe flag must be reported as applied")
	assert.Contains(t, gotArgs, "--dry-run")
	assert.Contains(t, gotArgs, "--manifests-no-prune")
	assert.True(t, res.Executed)
}

func TestRunSurfacesInventoryDelta(t *testing.T) {
	runner := func(_ []string) ([]byte, error) { return nil, nil }
	inventory := func() ([]string, error) {
		return []string{
			"_kubelet-serving-cert-approver__Namespace",
			"kube-system_metrics-server__Deployment",
		}, nil
	}

	res, err := Run(Options{
		To:      "1.36.3",
		Desired: []string{"kube-system_metrics-server__Deployment"},
	}, runner, inventory)

	require.NoError(t, err)
	assert.Equal(t, []string{"_kubelet-serving-cert-approver__Namespace"}, res.PruneCandidates)
}

// An unreadable inventory must not read as "nothing would be pruned" — that is
// the exact false negative that makes the flag feel unnecessary.
func TestRunInventoryFailureIsNotAnEmptyDelta(t *testing.T) {
	runner := func(_ []string) ([]byte, error) { return nil, nil }
	inventory := func() ([]string, error) {
		return nil, errors.New("configmaps \"talos-bootstrap-manifests-inventory\" not found")
	}

	res, err := Run(Options{To: "1.36.3"}, runner, inventory)

	require.NoError(t, err, "an unreadable inventory must not fail a safe dry run")
	assert.NotEmpty(t, res.InventoryWarning)
	assert.Nil(t, res.PruneCandidates)
}

func TestInventoryDelta(t *testing.T) {
	tests := []struct {
		name    string
		cluster []string
		desired []string
		want    []string
	}{
		{
			name:    "keys absent from the desired set are prune candidates",
			cluster: []string{"b", "a", "c"},
			desired: []string{"b"},
			want:    []string{"a", "c"},
		},
		{
			name:    "converged inventory yields no candidates",
			cluster: []string{"a", "b"},
			desired: []string{"a", "b"},
			want:    nil,
		},
		{
			name:    "empty cluster inventory yields no candidates",
			cluster: nil,
			desired: []string{"a"},
			want:    nil,
		},
		{
			name:    "empty desired set makes every key a candidate",
			cluster: []string{"a", "b"},
			desired: nil,
			want:    []string{"a", "b"},
		},
		{
			name:    "whitespace and blanks are ignored on both sides",
			cluster: []string{" a ", "", "b"},
			desired: []string{"a", "  "},
			want:    []string{"b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Sorted output so the report is stable run to run.
			assert.Equal(t, tt.want, InventoryDelta(tt.cluster, tt.desired))
		})
	}
}

func TestParseInventoryKeys(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "jsonpath data map yields sorted keys",
			data: `{"_kubelet-serving-cert-approver__Namespace":"sha256:aaa","kube-system_coredns__Deployment":"sha256:bbb"}`,
			want: []string{"_kubelet-serving-cert-approver__Namespace", "kube-system_coredns__Deployment"},
		},
		{
			name: "empty data map yields no keys",
			data: `{}`,
			want: nil,
		},
		{
			name: "blank output yields no keys",
			data: "  \n",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInventoryKeys([]byte(tt.data))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseInventoryKeysRejectsGarbage(t *testing.T) {
	_, err := ParseInventoryKeys([]byte("not json at all"))
	require.Error(t, err)
}

func TestReportIsTOONAndNamesThePruneDecision(t *testing.T) {
	res := Result{
		To:               "1.36.3",
		Node:             "192.168.1.50",
		DryRun:           true,
		ManifestsNoPrune: true,
		Executed:         true,
		DesiredSupplied:  true,
		Args:             []string{"-n", "192.168.1.50", "upgrade-k8s", "--to", "1.36.3", "--manifests-no-prune", "--dry-run"},
		PruneCandidates:  []string{"_kubelet-serving-cert-approver__Namespace"},
	}

	out := Report(res)

	assert.Contains(t, out, "mode: dry-run")
	assert.Contains(t, out, "manifests_no_prune: true")
	// The delta is the signal an operator would otherwise assemble by hand, so
	// it must be counted, not just listed.
	assert.Contains(t, out, "prune_candidates[1]{key}:")
	assert.Contains(t, out, "_kubelet-serving-cert-approver__Namespace")
	assert.Contains(t, out, "help[")
	// Progress chatter on stdout would be read as data by an agent.
	assert.NotContains(t, out, "Fetching")
}

func TestReportDefinitiveEmptyState(t *testing.T) {
	out := Report(Result{
		To: "1.36.3", DryRun: true, ManifestsNoPrune: true, Executed: true,
		DesiredSupplied: true,
	})

	assert.Contains(t, out, "prune_candidates: 0 keys would be pruned")
	assert.NotContains(t, out, "prune_candidates[")
}

// Without a desired set there is no delta to compute, and reporting zero would
// be a verification claim the command never actually made.
func TestReportWillNotClaimZeroWithoutADesiredSet(t *testing.T) {
	out := Report(Result{
		To: "1.36.3", DryRun: true, ManifestsNoPrune: true, Executed: true,
		DesiredSupplied: false,
		Inventory:       []string{"_kubelet-serving-cert-approver__Namespace", "kube-system_coredns__Deployment"},
	})

	assert.NotContains(t, out, "0 keys would be pruned")
	assert.Contains(t, out, "desired set not supplied")
	// The inventory is still the useful part, so it is listed and counted.
	assert.Contains(t, out, "inventory[2]{key}:")
	assert.Contains(t, out, "--desired-from")
}

func TestRunRecordsWhetherDesiredWasSupplied(t *testing.T) {
	runner := func(_ []string) ([]byte, error) { return nil, nil }
	inventory := func() ([]string, error) { return []string{"a", "b"}, nil }

	without, err := Run(Options{To: "1.36.3"}, runner, inventory)
	require.NoError(t, err)
	assert.False(t, without.DesiredSupplied)
	assert.Equal(t, []string{"a", "b"}, without.Inventory)
	assert.Nil(t, without.PruneCandidates, "no desired set means no delta was computed")

	with, err := Run(Options{To: "1.36.3", Desired: []string{"a"}}, runner, inventory)
	require.NoError(t, err)
	assert.True(t, with.DesiredSupplied)
	assert.Equal(t, []string{"b"}, with.PruneCandidates)
}

func TestReportNamesTheRiskWhenPruneIsEnabled(t *testing.T) {
	out := Report(Result{
		To: "1.36.3", Apply: true, Executed: true,
		ManifestsNoPrune: false,
		PruneCandidates:  []string{"_kubelet-serving-cert-approver__Namespace"},
	})

	assert.Contains(t, out, "mode: apply")
	assert.Contains(t, out, "manifests_no_prune: false")
	assert.Contains(t, out, "warning:")
}

func TestReportSurfacesInventoryWarning(t *testing.T) {
	out := Report(Result{
		To: "1.36.3", DryRun: true, ManifestsNoPrune: true, Executed: true,
		InventoryWarning: "inventory unreadable: not found",
	})

	assert.Contains(t, out, "inventory_warning:")
	assert.Contains(t, out, "not found")
	// Unknown must never render as a confident zero.
	assert.NotContains(t, out, "0 keys would be pruned")
}

func TestUsageErrorRendersActionableHelpOnStdout(t *testing.T) {
	err := Validate(Options{To: "1.36.3", Apply: true, AllowManifestPrune: true})
	require.Error(t, err)

	out := ReportError(err)

	assert.True(t, strings.HasPrefix(out, "error:"), "AXI errors lead with error:")
	assert.Contains(t, out, "help:")
	assert.Contains(t, out, "--confirm-manifest-prune")
}
