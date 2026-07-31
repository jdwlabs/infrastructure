package haproxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffIsEmptyForIdenticalConfigs(t *testing.T) {
	cfg := "global\n    maxconn 32000\n"
	assert.Empty(t, Diff(cfg, cfg))
}

// The config is written from Go and read back through a shell. A line-ending or
// trailing-newline difference introduced by that round-trip is not drift, and
// reporting it as drift would make every plan propose a pointless push.
func TestDiffIgnoresLineEndingAndTrailingNewlineNoise(t *testing.T) {
	deployed := "global\r\n    maxconn 32000\r\n\r\n"
	rendered := "global\n    maxconn 32000\n"
	assert.Empty(t, Diff(deployed, rendered))
}

func TestDiffMarksChangedBackendLines(t *testing.T) {
	deployed := strings.Join([]string{
		"backend k8s-controlplane",
		"    balance leastconn",
		"    server talos-cp-201 192.168.1.21:6443 check",
		"    server talos-cp-202 192.168.1.22:6443 check",
	}, "\n")
	rendered := strings.Join([]string{
		"backend k8s-controlplane",
		"    balance leastconn",
		"    server talos-cp-201 192.168.1.21:6443 check",
		"    server talos-cp-202 192.168.1.99:6443 check",
	}, "\n")

	diff := Diff(deployed, rendered)
	require.NotEmpty(t, diff)
	assert.Contains(t, diff, "-    server talos-cp-202 192.168.1.22:6443 check")
	assert.Contains(t, diff, "+    server talos-cp-202 192.168.1.99:6443 check")
	assert.Contains(t, diff, "@@ ")
	assert.Contains(t, diff, "--- deployed "+ConfigPath)
}

func TestDiffReportsAdditionsAndRemovals(t *testing.T) {
	deployed := "a\nb\nc\n"
	rendered := "a\nc\nd\n"

	diff := Diff(deployed, rendered)
	assert.Contains(t, diff, "-b")
	assert.Contains(t, diff, "+d")
	assert.NotContains(t, diff, "-a", "unchanged lines are context, not edits")
}

func TestDiffAgainstAnEmptyDeployedConfig(t *testing.T) {
	diff := Diff("", "global\n    daemon\n")
	assert.Contains(t, diff, "+global")
	assert.Contains(t, diff, "+    daemon")
}

// A far-apart pair of edits belongs in two hunks; one hunk would drag every
// unchanged line between them into the output.
func TestDiffSplitsDistantEditsIntoSeparateHunks(t *testing.T) {
	var deployed, rendered []string
	for i := range 40 {
		deployed = append(deployed, string(rune('a'+i%26))+"-line")
		rendered = append(rendered, string(rune('a'+i%26))+"-line")
	}
	deployed[1] = "changed-near-top"
	rendered[38] = "changed-near-bottom"

	diff := Diff(strings.Join(deployed, "\n"), strings.Join(rendered, "\n"))
	assert.Equal(t, 2, strings.Count(diff, "@@ -"))
}

func TestTruncateCutsOnALineBoundary(t *testing.T) {
	diff := "line one\nline two\nline three\n"
	out, truncated := Truncate(diff, 12)

	assert.True(t, truncated)
	assert.Equal(t, "line one\n", out, "half a diff line reads as a different change")
}

func TestTruncateLeavesShortDiffsAlone(t *testing.T) {
	diff := "line one\n"
	out, truncated := Truncate(diff, 100)

	assert.False(t, truncated)
	assert.Equal(t, diff, out)
}

func TestHashIsStableAcrossLineEndingRoundTrips(t *testing.T) {
	assert.Equal(t, Hash("global\n    daemon\n"), Hash("global\r\n    daemon\r\n\r\n"))
	assert.NotEqual(t, Hash("global\n"), Hash("defaults\n"))
}
