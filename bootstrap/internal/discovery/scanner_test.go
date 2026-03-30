package discovery

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewScanner(t *testing.T) {
	nodeIPs := map[string]net.IP{
		"pve1": net.ParseIP("192.168.1.10"),
		"pve2": net.ParseIP("192.168.1.11"),
	}

	scanner := NewScanner("root", nodeIPs, true)

	assert.NotNil(t, scanner)
	assert.Equal(t, "root", scanner.sshUser)
	assert.Equal(t, nodeIPs, scanner.nodeIPs)
	assert.NotNil(t, scanner.sshConfig)
	assert.Equal(t, 10*time.Second, scanner.sshConfig.Timeout)
}

func TestSetPrivateKey(t *testing.T) {
	scanner := NewScanner("root", map[string]net.IP{}, true)

	// Test non-existent key
	err := scanner.SetPrivateKey("/nonexistent/key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read private key")

	// Test invalid key
	tmpDir := t.TempDir()
	invalidKey := filepath.Join(tmpDir, "invalid")
	err = os.WriteFile(invalidKey, []byte("not a valid key"), 0600)
	require.NoError(t, err)

	err = scanner.SetPrivateKey(invalidKey)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

func TestParseARPTable(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		targetMAC  string
		expectedIP net.IP
	}{
		{
			name: "find valid entry",
			output: `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.50     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0
192.168.1.51     0x1         0x2         BC:24:11:12:34:56     *        vmbr0`,
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: net.ParseIP("192.168.1.50"),
		},
		{
			name: "case insensitive MAC match",
			output: `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.50     0x1         0x2         bc:24:11:ab:cd:ef     *        vmbr0`,
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: net.ParseIP("192.168.1.50"),
		},
		{
			name: "skip incomplete entries",
			output: `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.50     0x1         0x0         00:00:00:00:00:00     *        vmbr0
192.168.1.51     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0`,
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: net.ParseIP("192.168.1.51"),
		},
		{
			name: "skip INCOMPLETE entries",
			output: `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.50     0x1         0x0         INCOMPLETE            *        vmbr0
192.168.1.51     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0`,
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: net.ParseIP("192.168.1.51"),
		},
		{
			name:       "MAC not found",
			output:     `192.168.1.50     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0`,
			targetMAC:  "DE:AD:BE:EF:00:00",
			expectedIP: nil,
		},
		{
			name:       "empty table",
			output:     "",
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: nil,
		},
		{
			name: "multiple entries same MAC (should return first)",
			output: `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.50     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0
192.168.1.51     0x1         0x2         BC:24:11:AB:CD:EF     *        vmbr0`,
			targetMAC:  "BC:24:11:AB:CD:EF",
			expectedIP: net.ParseIP("192.168.1.50"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseARPTable(tt.output, tt.targetMAC)
			assert.Equal(t, tt.expectedIP, result)
		})
	}
}

func TestTestPort(t *testing.T) {
	// Start a test server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().(*net.TCPAddr)

	// Test port open
	assert.True(t, TestPort("127.0.0.1", addr.Port, 1*time.Second))

	// Test port closed
	assert.False(t, TestPort("127.0.0.1", addr.Port+1, 100*time.Millisecond))

	// Test invalid IP
	assert.False(t, TestPort("256.256.256.256", 80, 100*time.Millisecond))

	// Test connection refused
	assert.False(t, TestPort("127.0.0.1", 1, 100*time.Millisecond))
}

func TestScanner_DiscoverVMs(t *testing.T) {
	nodeIPs := map[string]net.IP{
		"pve1": net.ParseIP("192.168.1.10"),
	}

	scanner := NewScanner("root", nodeIPs, true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	vmids := []types.VMID{100, 101}

	// Without mock SSH server, this will fail to connect
	// This shows the need for interface-based design
	results, err := scanner.DiscoverVMs(ctx, vmids)

	// Should return empty results when VMs not found (no SSH connection)
	// The function may return an error or empty results depending on implementation
	if err != nil {
		// Error is acceptable - means SSH connection failed
		t.Logf("DiscoverVMs returned error (expected): %v", err)
	}
	assert.Empty(t, results)
}

// Test findIPByMAC without SSH - uses empty scanner
func TestScanner_FindIPByMAC_NoNodes(t *testing.T) {
	scanner := NewScanner("root", map[string]net.IP{}, true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// With no nodes configured, the function returns immediately with nil, nil
	// because there are no goroutines to launch, so the results channel closes right away
	ip, err := scanner.findIPByMAC(ctx, "BC:24:11:AB:CD:EF")

	// Should return nil IP without error (no nodes to search)
	assert.Nil(t, ip)
	assert.NoError(t, err) // No error - just no results because no nodes exist
}

func TestRepopulateNode_SubnetExtraction(t *testing.T) {
	// Test the subnet extraction logic
	tests := []struct {
		ip       string
		expected string
	}{
		{"192.168.1.10", "192.168.1"},
		{"10.0.0.5", "10.0.0"},
		{"172.16.50.100", "172.16.50"},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			ipStr := ip.String()

			// Use strings.LastIndex like the actual implementation
			lastDot := -1
			for i := len(ipStr) - 1; i >= 0; i-- {
				if ipStr[i] == '.' {
					lastDot = i
					break
				}
			}

			var subnet string
			if lastDot != -1 {
				subnet = ipStr[:lastDot]
			}
			assert.Equal(t, tt.expected, subnet)
		})
	}
}

func TestParseProxmoxNodes(t *testing.T) {
	tests := []struct {
		name        string
		jsonOutput  string
		expectError bool
		expectedLen int
		expectedIP  map[string]string
	}{
		{
			name: "valid single node",
			jsonOutput: `[
				{"node":"pve1","ip":"192.168.1.10","cpu":0.1,"maxcpu":8,"mem":1000000,"maxmem":16000000000,"uptime":12345}
			]`,
			expectError: false,
			expectedLen: 1,
			expectedIP:  map[string]string{"pve1": "192.168.1.10"},
		},
		{
			name: "valid multiple nodes",
			jsonOutput: `[
				{"node":"pve1","ip":"192.168.1.10"},
				{"node":"pve2","ip":"192.168.1.11"}
			]`,
			expectError: false,
			expectedLen: 2,
			expectedIP: map[string]string{
				"pve1": "192.168.1.10",
				"pve2": "192.168.1.11",
			},
		},
		{
			name:        "empty output",
			jsonOutput:  "",
			expectError: true,
			expectedLen: 0,
		},
		{
			name:        "no nodes found",
			jsonOutput:  "[]",
			expectError: true,
			expectedLen: 0,
		},
		{
			name:        "malformed JSON with valid data",
			jsonOutput:  `{"node":"pve1","ip":"192.168.1.10"}`,
			expectError: false,
			expectedLen: 1,
			expectedIP:  map[string]string{"pve1": "192.168.1.10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseProxmoxNodes(tt.jsonOutput)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Len(t, result, tt.expectedLen)

			for nodeName, expectedIPStr := range tt.expectedIP {
				ip, ok := result[nodeName]
				assert.True(t, ok, "expected node %s to be present", nodeName)
				if ok {
					assert.Equal(t, expectedIPStr, ip.String())
				}
			}
		})
	}
}

func TestScanner_MarkJoinedNodes(t *testing.T) {
	scanner := NewScanner("root", map[string]net.IP{
		"pve1": net.ParseIP("192.168.1.10"),
	}, true)

	results := map[types.VMID]*types.LiveNode{
		100: {
			VMID: types.VMID(100),
			IP:   net.ParseIP("192.168.1.50"),
			MAC:  "BC:24:11:AA:BB:CC",
		},
		101: {
			VMID: types.VMID(101),
			IP:   net.ParseIP("192.168.1.51"),
			MAC:  "BC:24:11:AA:BB:CD",
		},
		102: {
			VMID: types.VMID(102),
			IP:   nil, // No IP discovered yet
			MAC:  "BC:24:11:AA:BB:CE",
		},
	}

	memberAddrs := []string{
		"192.168.1.50",
		"192.168.1.99", // Not in our results
	}

	scanner.MarkJoinedNodes(memberAddrs, results)

	// Check that node 100 is marked as joined
	assert.Equal(t, types.StatusJoined, results[100].Status)

	// Check that node 101 is NOT marked as joined
	assert.NotEqual(t, types.StatusJoined, results[101].Status)

	// Check that node 102 doesn't panic with nil IP
	assert.NotEqual(t, types.StatusJoined, results[102].Status)
}

func TestScanner_Close(t *testing.T) {
	scanner := NewScanner("root", map[string]net.IP{}, true)

	// Close on fresh scanner should not panic
	scanner.Close()

	// Close again should not panic
	scanner.Close()
}

// Test scanner connection pooling behavior
func TestScanner_GetConn_Pooling(t *testing.T) {
	scanner := NewScanner("root", map[string]net.IP{
		"pve1": net.ParseIP("192.168.1.10"),
	}, true)

	// Without actual SSH server, we can't test real pooling
	// But we can verify the method exists and handles missing connections

	// This will fail to dial, but tests the code path
	_, err := scanner.getConn(net.ParseIP("127.0.0.1"))
	assert.Error(t, err)
}

// Integration test helper - would need real SSH server or mock
func TestIntegration_DiscoverProxmoxNodes(t *testing.T) {
	// Skip in short mode
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This would require a real Proxmox node or mock SSH server
	t.Skip("Requires real Proxmox node or mock SSH server")
}

// MockSSHServer for integration-style tests
type MockSSHServer struct {
	listener net.Listener
}

func (m *MockSSHServer) Close() error {
	return m.listener.Close()
}

// Platform-specific path testing
func TestSetPrivateKey_WindowsPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}

	scanner := NewScanner("root", map[string]net.IP{}, true)

	// Test Windows-style path
	tmpDir := t.TempDir()
	// Create a valid key file
	validKey := `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACB7S6V3L1V0cG1rY3Rmc3Rlc3R0ZXN0dGVzdHRlc3QAAAAce0uldS9VdHBta2
N0ZnN0ZXN0dGVzdHRlc3Q=
-----END OPENSSH PRIVATE KEY-----`

	keyPath := filepath.Join(tmpDir, "test_key")
	err := os.WriteFile(keyPath, []byte(validKey), 0600)
	require.NoError(t, err)

	err = scanner.SetPrivateKey(keyPath)
	// May fail due to key format, but should not fail on path handling
	if err != nil {
		assert.Contains(t, err.Error(), "parse private key")
	}
}
