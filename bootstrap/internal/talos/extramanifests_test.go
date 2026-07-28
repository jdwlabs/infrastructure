package talos

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtraManifestsArePinned is the CI-enforced check: every
// cluster.extraManifests URL in the vendored Talos patches must be pinned to
// a commit SHA or version tag, or be recorded in patches/manifest-pin-allowlist.yaml
// with a reason. This runs under this package's normal `go test`, so no
// separate CI job is needed — it is picked up by the existing bootstrap.yml
// "test" job's `go test -race` step.
//
// Zero references is a legitimate end state — the patches deliberately ship an
// empty extraManifests block — so this must not assert that the live config
// contains any. TestExtractExtraManifests and the TestCheckAllExtraManifestPins_*
// cases cover the extraction and reporting paths against literal fixtures
// instead; asserting it here would pin a test to an operational fact that is
// allowed to change.
func TestExtraManifestsArePinned(t *testing.T) {
	sources := map[string]string{
		"patches/control-plane.yaml": controlPlanePatchTemplate,
		"patches/worker.yaml":        workerPatchTemplate,
	}

	_, violations, stale, err := CheckAllExtraManifestPins(sources)
	require.NoError(t, err)

	for name, list := range violations {
		assert.Emptyf(t, list, "%s: unpinned extraManifests URL with no allowlist entry: %v", name, list)
	}
	assert.Emptyf(t, stale, "stale manifest-pin-allowlist.yaml entries (no longer match an unpinned URL): %v", stale)
}

// The live patches carry no extraManifests at all, so the empty case needs a
// synthetic fixture to be exercised deliberately: the guard has to report it
// as clean rather than as a missing-fixture failure.
//
// Every assertion here checks for absence, so a guard gutted to return nothing
// would pass this too — the unpinned and stale cases below are what hold its
// behaviour in place.
func TestCheckAllExtraManifestPins_EmptySetIsClean(t *testing.T) {
	withAllowlist(t, "exceptions: []\n")

	sources := map[string]string{
		"patches/control-plane.yaml": `cluster:
  apiServer:
    extraArgs:
      bind-address: 0.0.0.0
`,
		"patches/worker.yaml": "machine:\n  type: worker\n",
	}

	refs, violations, stale, err := CheckAllExtraManifestPins(sources)
	require.NoError(t, err)

	for name, list := range refs {
		assert.Emptyf(t, list, "%s: expected no extraManifests references", name)
	}
	for name, list := range violations {
		assert.Emptyf(t, list, "%s: empty extraManifests must not be a violation: %v", name, list)
	}
	assert.Empty(t, stale, "an empty allowlist alongside an empty manifest set is not stale")
}

// TestCheckAllExtraManifestPins_UnpinnedStillFails proves tolerating the empty
// set did not defang the guard: an unpinned entry with no allowlist cover is
// still reported.
func TestCheckAllExtraManifestPins_UnpinnedStillFails(t *testing.T) {
	withAllowlist(t, "exceptions: []\n")

	sources := map[string]string{
		"patches/control-plane.yaml": `cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml
    - https://raw.githubusercontent.com/owner/repo2/v1.2.3/deploy/install.yaml
`,
	}

	_, violations, stale, err := CheckAllExtraManifestPins(sources)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml",
	}, violations["patches/control-plane.yaml"])
	assert.Empty(t, stale)
}

// TestCheckAllExtraManifestPins_StaleAllowlistEntryFails proves an allowlist
// entry matching nothing is still rejected, including when the manifest set is
// empty — that combination is exactly how the allowlist rots.
func TestCheckAllExtraManifestPins_StaleAllowlistEntryFails(t *testing.T) {
	withAllowlist(t, `exceptions:
  - url: https://raw.githubusercontent.com/owner/gone/main/deploy/install.yaml
    reason: no longer referenced by any patch
`)

	sources := map[string]string{
		"patches/control-plane.yaml": "machine:\n  type: controlplane\n",
	}

	_, _, stale, err := CheckAllExtraManifestPins(sources)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"https://raw.githubusercontent.com/owner/gone/main/deploy/install.yaml",
	}, stale)
}

// withAllowlist swaps the embedded allowlist for the duration of a test so the
// guard's behaviour can be exercised independently of the live file's contents.
func withAllowlist(t *testing.T, yaml string) {
	t.Helper()
	orig := manifestPinAllowlistYAML
	t.Cleanup(func() { manifestPinAllowlistYAML = orig })
	manifestPinAllowlistYAML = yaml
}

func TestExtractExtraManifests(t *testing.T) {
	source := `machine:
  type: controlplane
cluster:
  apiServer:
    extraArgs:
      foo: bar
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml
    - https://raw.githubusercontent.com/owner/repo2/v1.2.3/deploy/install.yaml
  allowSchedulingOnControlPlanes: false
`
	urls := ExtractExtraManifests(source)
	assert.Equal(t, []string{
		"https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml",
		"https://raw.githubusercontent.com/owner/repo2/v1.2.3/deploy/install.yaml",
	}, urls)
}

func TestClassifyManifestRef(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantRef    string
		wantPinned bool
	}{
		{
			name:       "branch ref is unpinned",
			url:        "https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml",
			wantRef:    "main",
			wantPinned: false,
		},
		{
			name:       "master is unpinned",
			url:        "https://raw.githubusercontent.com/owner/repo/master/deploy/install.yaml",
			wantRef:    "master",
			wantPinned: false,
		},
		{
			name:       "semver tag is pinned",
			url:        "https://raw.githubusercontent.com/owner/repo/v1.2.3/deploy/install.yaml",
			wantRef:    "v1.2.3",
			wantPinned: true,
		},
		{
			name:       "full commit sha is pinned",
			url:        "https://raw.githubusercontent.com/owner/repo/1789c3a341ba25e28d1a964864e630230ff77128/deploy/install.yaml",
			wantRef:    "1789c3a341ba25e28d1a964864e630230ff77128",
			wantPinned: true,
		},
		{
			name:       "short sha-like ref is not treated as pinned",
			url:        "https://raw.githubusercontent.com/owner/repo/1789c3a/deploy/install.yaml",
			wantRef:    "1789c3a",
			wantPinned: false,
		},
		{
			name:       "unrecognised URL shape is conservatively unpinned",
			url:        "https://example.com/some/other/layout.yaml",
			wantRef:    "",
			wantPinned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, pinned := ClassifyManifestRef(tc.url)
			assert.Equal(t, tc.wantRef, ref)
			assert.Equal(t, tc.wantPinned, pinned)
		})
	}
}

// TestCheckExtraManifestPins_CatchesViolation proves the check fails closed:
// an unpinned URL with no matching allowlist entry is reported as a
// violation, not silently passed.
func TestCheckExtraManifestPins_CatchesViolation(t *testing.T) {
	source := `cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml
`
	_, violations := CheckExtraManifestPins(source, map[string]string{})
	require.Len(t, violations, 1)
	assert.Equal(t, "https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml", violations[0])
}

func TestLoadManifestPinAllowlist_RejectsMissingReason(t *testing.T) {
	withAllowlist(t, `exceptions:
  - url: https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml
    reason: ""
`)

	_, err := LoadManifestPinAllowlist()
	assert.Error(t, err)
}
