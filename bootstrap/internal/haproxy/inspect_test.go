package haproxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceStateReadsTheUnitState(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("systemctl is-active haproxy", "active\n", 0)

	state, err := createTestClient(t, server).ServiceState(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "active", state)
}

// `systemctl is-active` prints the state and exits 3 when the unit is not
// running. Treating the non-zero exit as an inspection failure would turn a
// known state into an unknown one.
func TestServiceStateReadsAnInactiveUnitDespiteNonZeroExit(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("systemctl is-active haproxy", "inactive\n", 3)

	state, err := createTestClient(t, server).ServiceState(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "inactive", state)
}

func TestServiceStateFailsWhenTheHostSaysNothing(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetNextError(errors.New("connection reset"))

	_, err := createTestClient(t, server).ServiceState(context.Background())
	require.Error(t, err)
}

func TestStatsParsesTheRuntimeSocketResponse(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse(`echo "show stat"`, sampleStatsCSV, 0)

	stats, err := createTestClient(t, server).Stats(context.Background())
	require.NoError(t, err)
	require.Len(t, stats, 3)
	assert.Equal(t, "talos-cp-201", stats[0].Name())
	assert.Equal(t, 2, UpCount(stats))
}

// A host without socat must produce a named failure, not an empty backend list
// that reads as "HAProxy has no servers".
func TestStatsFailsRatherThanReportingNoBackends(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse(`echo "show stat"`, "sudo: nc: command not found\n", 127)

	_, err := createTestClient(t, server).Stats(context.Background())
	require.Error(t, err)
}

func TestStatsCommandTriesSocatBeforeNetcat(t *testing.T) {
	socatIdx := strings.Index(showStatCmd, "socat")
	ncIdx := strings.Index(showStatCmd, " nc ")
	require.NotEqual(t, -1, socatIdx)
	require.NotEqual(t, -1, ncIdx)
	assert.Less(t, socatIdx, ncIdx)
	assert.Contains(t, showStatCmd, statsSocket)
}

func TestDeployedConfigReadsTheInstalledFile(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("sudo cat "+ConfigPath, "global\n    daemon\n", 0)

	cfg, err := createTestClient(t, server).DeployedConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "global\n    daemon\n", cfg)
}

func TestDeployedConfigSurfacesAPermissionFailure(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("sudo cat "+ConfigPath, "Permission denied\n", 1)

	_, err := createTestClient(t, server).DeployedConfig(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), ConfigPath)
}

func TestKeepalivedStateReadsTheUnitState(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("systemctl is-active keepalived", "inactive\n", 3)

	state, err := createTestClient(t, server).KeepalivedState(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "inactive", state)
}

const addrsHoldingVIP = `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: eth0    inet 192.168.1.11/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever
2: eth0    inet 192.168.1.199/24 scope global secondary eth0\       valid_lft forever preferred_lft forever
`

func TestHoldsAddressFindsAVirtualAddressConfiguredOnTheHost(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("ip -o -4 addr show", addrsHoldingVIP, 0)

	holds, err := createTestClient(t, server).HoldsAddress(context.Background(), net.ParseIP("192.168.1.199"))
	require.NoError(t, err)
	assert.True(t, holds)
}

// 192.168.1.19 is a prefix of 192.168.1.199. A substring match without the
// prefix-length boundary would name this host the holder of an address it
// does not have.
func TestHoldsAddressDoesNotMatchAnAddressThatIsOnlyAPrefix(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("ip -o -4 addr show", addrsHoldingVIP, 0)

	holds, err := createTestClient(t, server).HoldsAddress(context.Background(), net.ParseIP("192.168.1.19"))
	require.NoError(t, err)
	assert.False(t, holds)
}

// "Not the holder" and "could not ask" must stay different answers: reporting
// an unreadable instance as a backup hides a possible second holder.
func TestHoldsAddressFailsRatherThanReportingNotHeld(t *testing.T) {
	server := newMockSSHServer(t)
	defer server.Close()
	server.SetResponse("ip -o -4 addr show", "sh: 1: ip: not found\n", 127)

	_, err := createTestClient(t, server).HoldsAddress(context.Background(), net.ParseIP("192.168.1.199"))
	require.Error(t, err)
}
