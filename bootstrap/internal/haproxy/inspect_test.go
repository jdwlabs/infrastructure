package haproxy

import (
	"context"
	"errors"
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
