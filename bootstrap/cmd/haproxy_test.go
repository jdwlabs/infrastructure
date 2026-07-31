package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every case below is decided before any SSH connection or Terraform read, so
// these run with no load balancer, no cluster, and no network.

// A dropped flag is worse than an error: the caller gets plausible output it
// believes is scoped the way it asked for.
func TestHAProxyRejectsUnknownFlagAsStructuredOutput(t *testing.T) {
	cmd := haproxyCmd(setupTestApp(t))

	out, err := execute(t, cmd, "status", "--stat", "up")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "error: {code: unknown_flag")
	assert.Contains(t, out, "--stat")
	assert.Contains(t, out, "help[1]:", "the correction belongs inline, not behind a follow-up --help")
	assert.Contains(t, out, "--host")
	assert.Contains(t, out, "--fields")
}

func TestHAProxyRejectsPositionalArgs(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"status", "unexpected"}, {"plan", "unexpected"}, {"apply", "unexpected"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, haproxyCmd(setupTestApp(t)), args...)
			require.Error(t, err)
		})
	}
}

func TestHAProxyRejectsANonAddressHost(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "status", "--host", "haproxy-1.local")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "haproxy_host_invalid")
	assert.Contains(t, out, "is not an IP address")
}

// A refusal must report which layers it could not read rather than implying
// they were checked and found healthy.
func TestHAProxyStatusRefusalStillReportsUnknownLayers(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "status", "--host", "not-an-ip")

	require.Error(t, err)
	assert.Contains(t, out, "ssh: unknown")
	assert.Contains(t, out, "service: unknown")
	assert.Contains(t, out, "configDrift: unknown")
	assert.Contains(t, out, "state: unknown")
}

// A cluster with no recorded control planes has no backends to render, and that
// is decided before anything reaches the load balancer.
func TestHAProxyPlanRefusesWithoutControlPlanes(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "plan")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "no_control_planes")
	assert.Contains(t, out, "talops reconcile --plan")
}

func TestHAProxyApplyRefusesWithoutControlPlanesBeforeTouchingTheHost(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "apply")

	require.Error(t, err)
	assert.Contains(t, out, "no_control_planes")
	assert.Contains(t, out, "changed: false")
}

func TestHAProxyJSONRefusalIsOneObjectTaggedWithItsCommand(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "plan", "--json")

	require.Error(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], `"event":"plan"`)
	assert.Contains(t, lines[0], `"code":"no_control_planes"`)
}

// AXI's content-first rule: a bare invocation shows live state, because a
// caller can act on a health report but must make a second call after help text.
func TestBareHAProxyGroupShowsStateAndIdentifiesItself(t *testing.T) {
	out, _ := execute(t, haproxyCmd(setupTestApp(t)))

	assert.Contains(t, out, "bin: ")
	assert.Contains(t, out, "description: ")
	assert.Contains(t, out, "haproxy:", "the bare group reports status, not usage")
	assert.NotContains(t, out, "Usage:")
}

func TestHAProxyHelpStatesThatNothingHereMutatesProxmox(t *testing.T) {
	out, err := execute(t, haproxyCmd(setupTestApp(t)), "--help")

	require.NoError(t, err)
	assert.Contains(t, out, "never mutates Proxmox")
	assert.Contains(t, out, "status")
	assert.Contains(t, out, "plan")
	assert.Contains(t, out, "apply")
}

func TestHAProxySubcommandHelpCarriesExamples(t *testing.T) {
	for _, sub := range []string{"status", "plan", "apply"} {
		t.Run(sub, func(t *testing.T) {
			out, err := execute(t, haproxyCmd(setupTestApp(t)), sub, "--help")

			require.NoError(t, err)
			assert.Contains(t, out, "Examples:")
			assert.Contains(t, out, "talops haproxy "+sub)
			assert.Contains(t, out, "--host")
		})
	}
}

// Exit code 2 is claimed by two different contracts in this repo. Nothing in
// this group uses it: a refusal and a failure are both 1.
func TestHAProxyNeverExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--stat"},
		{"status", "--host", "nope"},
		{"plan"},
		{"apply"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, haproxyCmd(setupTestApp(t)), args...)
			require.Error(t, err)
			assert.NotEqual(t, 2, ExitCode(err))
			assert.Equal(t, 1, ExitCode(err))
		})
	}
}
