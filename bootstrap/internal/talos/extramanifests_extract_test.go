package talos

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractExtraManifests_Shapes covers the YAML shapes a line-oriented
// scanner silently dropped. A dropped URL is a guard bypass, not a cosmetic
// miss: the pin check only ever sees what extraction returns, so an entry it
// never yields is an unpinned manifest that passes CI.
func TestExtractExtraManifests_Shapes(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name: "trailing comment on the item",
			source: `cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml # reused
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "flow sequence on the header line",
			source: `cluster:
  extraManifests: [https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml]
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "multi-entry flow sequence",
			source: `cluster:
  extraManifests: [https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml, https://raw.githubusercontent.com/owner/repo2/v1.2.3/deploy/b.yaml]
`,
			want: []string{
				"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml",
				"https://raw.githubusercontent.com/owner/repo2/v1.2.3/deploy/b.yaml",
			},
		},
		{
			name: "comment line at header indent inside the block",
			source: `cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/v1.2.3/deploy/a.yaml
  # why the next one is here
    - https://raw.githubusercontent.com/owner/repo/main/deploy/b.yaml
`,
			want: []string{
				"https://raw.githubusercontent.com/owner/repo/v1.2.3/deploy/a.yaml",
				"https://raw.githubusercontent.com/owner/repo/main/deploy/b.yaml",
			},
		},
		{
			name: "quoted items",
			source: `cluster:
  extraManifests:
    - "https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"
    - 'https://raw.githubusercontent.com/owner/repo2/main/deploy/b.yaml'
`,
			want: []string{
				"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml",
				"https://raw.githubusercontent.com/owner/repo2/main/deploy/b.yaml",
			},
		},
		{
			name: "extra whitespace after the dash",
			source: `cluster:
  extraManifests:
    -   https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "block scalar folded url",
			source: `cluster:
  extraManifests:
    - >-
      https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "second document in a multi-document patch",
			source: `machine:
  type: worker
---
cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "entries reached through an anchor alias",
			source: `manifests: &shared
  - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
cluster:
  extraManifests: *shared
`,
			want: []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		},
		{
			name: "url mentioned only in a comment is not an entry",
			source: `cluster:
  # https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
  extraManifests: []
`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractExtraManifests(tc.source))
		})
	}
}

// TestCheckExtraManifestPins_BypassShapesAreViolations proves the shapes above
// reach the guard end-to-end rather than just the extractor: each unpinned URL
// must be reported as a violation.
func TestCheckExtraManifestPins_BypassShapesAreViolations(t *testing.T) {
	sources := []string{
		"cluster:\n  extraManifests:\n    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml # reused\n",
		"cluster:\n  extraManifests: [https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml]\n",
		"cluster:\n  extraManifests:\n  # note\n    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml\n",
	}

	for _, source := range sources {
		_, violations := CheckExtraManifestPins(source, map[string]string{})
		assert.Equalf(t, []string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"}, violations,
			"unpinned URL must be reported for source:\n%s", source)
	}
}

// TestExtractExtraManifests_TemplatedPatches guards the reason the original
// implementation avoided a YAML parse: the vendored patches are Go
// text/template sources containing `{{ ... }}` actions in scalar position and
// are not valid YAML as written. Extraction must still work on them.
func TestExtractExtraManifests_TemplatedPatches(t *testing.T) {
	for name, source := range map[string]string{
		"patches/control-plane.yaml": controlPlanePatchTemplate,
		"patches/worker.yaml":        workerPatchTemplate,
	} {
		assert.Emptyf(t, ExtractExtraManifests(source), "%s: expected no extraManifests entries", name)
	}

	templated := `machine:
  install:
    disk: /dev/{{ .DefaultDisk }}
  network:
    interfaces:
      - interface: {{ .DefaultNetworkInterface }}
cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
`
	assert.Equal(t,
		[]string{"https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml"},
		ExtractExtraManifests(templated),
	)
}

// TestExtractExtraManifests_UnparseableSourceFailsClosed pins the fallback:
// a source no YAML parser can read must still surface its manifest URLs, so a
// malformed patch cannot launder an unpinned reference past the guard.
func TestExtractExtraManifests_UnparseableSourceFailsClosed(t *testing.T) {
	source := `cluster:
  extraManifests:
    - https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml
   :	tab and a stray colon that no parser accepts
  - [unbalanced
`
	_, err := parseExtraManifests(source)
	require.Error(t, err, "fixture must actually be unparseable or this proves nothing")

	urls := ExtractExtraManifests(source)
	require.NotEmpty(t, urls, "an unparseable source must not silently yield zero URLs")
	assert.Contains(t, urls, "https://raw.githubusercontent.com/owner/repo/main/deploy/a.yaml")
}

// TestClassifyManifestRef_AnchoredSemver covers refs that only look like
// versions. An unanchored version pattern let a branch whose name merely
// starts with digits classify as pinned.
func TestClassifyManifestRef_AnchoredSemver(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		wantPinned bool
	}{
		{name: "branch named like a version suffixed with a word", ref: "1.0-latest", wantPinned: false},
		{name: "two segment branch or release line", ref: "2.0", wantPinned: false},
		{name: "prerelease-shaped ref counts as a tag", ref: "1.2.3-branch-of-mine", wantPinned: true},
		{name: "version with trailing path-like junk", ref: "1.2.3junk", wantPinned: false},
		{name: "plain semver tag", ref: "1.2.3", wantPinned: true},
		{name: "v-prefixed semver tag", ref: "v1.2.3", wantPinned: true},
		{name: "prerelease tag", ref: "v1.2.3-rc.1", wantPinned: true},
		{name: "build metadata tag", ref: "v1.2.3+build.5", wantPinned: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, pinned := ClassifyManifestRef("https://raw.githubusercontent.com/owner/repo/" + tc.ref + "/deploy/install.yaml")
			assert.Equal(t, tc.ref, ref)
			assert.Equal(t, tc.wantPinned, pinned)
		})
	}
}
