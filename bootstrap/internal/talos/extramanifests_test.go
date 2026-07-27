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
func TestExtraManifestsArePinned(t *testing.T) {
	sources := map[string]string{
		"patches/control-plane.yaml": controlPlanePatchTemplate,
		"patches/worker.yaml":        workerPatchTemplate,
	}

	refs, violations, stale, err := CheckAllExtraManifestPins(sources)
	require.NoError(t, err)

	for name, list := range violations {
		assert.Emptyf(t, list, "%s: unpinned extraManifests URL with no allowlist entry: %v", name, list)
	}
	assert.Emptyf(t, stale, "stale manifest-pin-allowlist.yaml entries (no longer match an unpinned URL): %v", stale)

	// Sanity check the fixtures actually exercised the extraction path —
	// a check that silently finds zero references proves nothing.
	total := 0
	for _, list := range refs {
		total += len(list)
	}
	assert.Greater(t, total, 0, "expected at least one cluster.extraManifests reference across the patch fixtures")
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
	orig := manifestPinAllowlistYAML
	defer func() { manifestPinAllowlistYAML = orig }()

	manifestPinAllowlistYAML = `exceptions:
  - url: https://raw.githubusercontent.com/owner/repo/main/deploy/install.yaml
    reason: ""
`
	_, err := LoadManifestPinAllowlist()
	assert.Error(t, err)
}
