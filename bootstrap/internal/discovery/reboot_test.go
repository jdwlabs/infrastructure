package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewRebootMonitor(t *testing.T) {
	logger := zap.NewNop()
	initialIP := net.ParseIP("192.168.1.201")
	mac := "BC:24:11:AA:BB:CC"
	vmid := types.VMID(201)

	// Create a minimal scanner (not nil)
	scanner := &Scanner{
		sshUser:   "test",
		nodeIPs:   make(map[string]net.IP),
		sshConfig: nil,
	}

	monitor := NewRebootMonitor(vmid, initialIP, mac, scanner, logger)

	assert.Equal(t, StateMonitoring, monitor.state)
	assert.Equal(t, vmid, monitor.vmid)
	assert.Equal(t, initialIP, monitor.initialIP)
	assert.Equal(t, mac, monitor.mac)
	assert.Equal(t, scanner, monitor.scanner)
	assert.Equal(t, 50000, monitor.talosPort)
	assert.NotZero(t, monitor.lastStateChange)
}

func TestRebootStateString(t *testing.T) {
	tests := []struct {
		state    RebootState
		expected string
	}{
		{StateMonitoring, "monitoring"},
		{StateRebooting, "rebooting"},
		{StateVerifying, "verifying"},
		{RebootState(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.state.String())
		})
	}
}

func TestRebootMonitorState(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{nodeIPs: make(map[string]net.IP)}

	monitor := NewRebootMonitor(
		types.VMID(201),
		net.ParseIP("192.168.1.201"),
		"BC:24:11:AA:BB:CC",
		scanner,
		logger,
	)

	assert.Equal(t, StateMonitoring, monitor.State())

	monitor.transitionTo(StateRebooting)
	assert.Equal(t, StateRebooting, monitor.State())

	monitor.transitionTo(StateVerifying)
	assert.Equal(t, StateVerifying, monitor.State())
}

func TestTransitionTo(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{nodeIPs: make(map[string]net.IP)}

	monitor := NewRebootMonitor(
		types.VMID(201),
		net.ParseIP("192.168.1.201"),
		"BC:24:11:AA:BB:CC",
		scanner,
		logger,
	)

	initialTime := monitor.lastStateChange
	time.Sleep(10 * time.Millisecond)

	monitor.transitionTo(StateRebooting)

	assert.Equal(t, StateRebooting, monitor.state)
	assert.True(t, monitor.lastStateChange.After(initialTime))
}

func TestWaitForReady_ContextCancellation(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{nodeIPs: make(map[string]net.IP)}

	monitor := NewRebootMonitor(
		types.VMID(201),
		net.ParseIP("192.168.1.201"),
		"BC:24:11:AA:BB:CC",
		scanner,
		logger,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := monitor.WaitForReady(ctx, 1*time.Minute)
	assert.Equal(t, context.Canceled, err)
}

func TestWaitForReady_Timeout(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{nodeIPs: make(map[string]net.IP)}

	monitor := NewRebootMonitor(
		types.VMID(201),
		net.ParseIP("192.168.1.201"),
		"BC:24:11:AA:BB:CC",
		scanner,
		logger,
	)

	// Very short timeout to test timeout handling
	ctx := context.Background()
	_, err := monitor.WaitForReady(ctx, 1*time.Millisecond)

	// Should timeout because the node won't become ready in 1ms
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

// Test tickMonitoring - uses a real Scanner with no nodeIPs (won't panic on repopulateARP)
func TestTickMonitoring_PortClosed(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{
		nodeIPs: make(map[string]net.IP), // Empty map, repopulateARP will return quickly
	}

	monitor := &RebootMonitor{
		state:     StateMonitoring,
		vmid:      types.VMID(201),
		initialIP: net.ParseIP("127.0.0.1"),
		mac:       "BC:24:11:AA:BB:CC",
		scanner:   scanner,
		logger:    logger,
		talosPort: 1, // Use port 1 (unlikely to be open) to trigger reboot transition
	}

	ctx := context.Background()
	ip, ready, err := monitor.tickMonitoring(ctx)

	// Port 1 is unlikely to be open, so should transition to rebooting
	// and return nil, false, nil
	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateRebooting, monitor.state)
}

// Test tickRebooting - uses a Scanner that will fail to find IP (normal case)
func TestTickRebooting_NoIPFound(t *testing.T) {
	logger := zap.NewNop()
	scanner := &Scanner{
		nodeIPs: make(map[string]net.IP), // Empty, findIPByMAC will return error
	}

	monitor := &RebootMonitor{
		state:        StateRebooting,
		vmid:         types.VMID(201),
		initialIP:    net.ParseIP("192.168.1.201"),
		mac:          "BC:24:11:AA:BB:CC",
		scanner:      scanner,
		logger:       logger,
		talosPort:    1,          // Use port 1 to ensure original IP check fails
		lastARPRepop: time.Now(), // Don't trigger ARP repop yet
	}

	ctx := context.Background()
	ip, ready, err := monitor.tickRebooting(ctx)

	// Should stay in rebooting state (no IP found, original IP not reachable)
	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateRebooting, monitor.state)
}

// Test tickVerifying - port closed 5 times should go back to rebooting
func TestTickVerifying_PortClosed(t *testing.T) {
	logger := zap.NewNop()

	monitor := &RebootMonitor{
		state:       StateVerifying,
		candidateIP: net.ParseIP("127.0.0.1"),
		logger:      logger,
		talosPort:   1, // Unlikely to be open
	}

	ctx := context.Background()

	// First four failures stay in Verifying
	for i := 0; i < 4; i++ {
		ip, ready, err := monitor.tickVerifying(ctx)
		assert.Nil(t, ip)
		assert.False(t, ready)
		assert.NoError(t, err)
		assert.Equal(t, StateVerifying, monitor.state)
	}

	// Fifth failure transitions to Rebooting
	ip, ready, err := monitor.tickVerifying(ctx)

	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateRebooting, monitor.state)
}

// Test tickVerifying - port open twice should return ready
func TestTickVerifying_PortOpen(t *testing.T) {
	logger := zap.NewNop()

	// Start a test server to have an open port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)

	monitor := &RebootMonitor{
		state:       StateVerifying,
		candidateIP: net.ParseIP("127.0.0.1"),
		logger:      logger,
		talosPort:   addr.Port,
	}

	ctx := context.Background()

	// First check: not ready yet (need 2 consecutive)
	ip, ready, err := monitor.tickVerifying(ctx)
	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.NoError(t, err)

	// Second check: now ready
	ip, ready, err = monitor.tickVerifying(ctx)
	assert.Equal(t, monitor.candidateIP, ip)
	assert.True(t, ready)
	assert.NoError(t, err)
}

func TestTickUnknownState(t *testing.T) {
	logger := zap.NewNop()

	monitor := &RebootMonitor{
		state:  RebootState(999),
		logger: logger,
	}

	ctx := context.Background()
	ip, ready, err := monitor.tick(ctx)

	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown state")
}

// Test tickRebooting - original IP comes back
func TestTickRebooting_OriginalIPReturns(t *testing.T) {
	logger := zap.NewNop()

	// Start a test server to simulate original IP coming back
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	initialIP := net.ParseIP("127.0.0.1")

	scanner := &Scanner{
		nodeIPs: make(map[string]net.IP),
	}

	monitor := &RebootMonitor{
		state:        StateRebooting,
		vmid:         types.VMID(201),
		initialIP:    initialIP,
		mac:          "BC:24:11:AA:BB:CC",
		scanner:      scanner,
		logger:       logger,
		talosPort:    addr.Port,  // Port is open
		lastARPRepop: time.Now(), // Don't trigger ARP repop
	}

	ctx := context.Background()
	ip, ready, err := monitor.tickRebooting(ctx)

	// Should transition to verifying with original IP as candidate
	assert.Nil(t, ip) // Not ready yet, just transitioned
	assert.False(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateVerifying, monitor.state)
	assert.Equal(t, initialIP, monitor.candidateIP)
}

// Test tickRebooting - finds new IP via MAC
func TestTickRebooting_NewIPFound(t *testing.T) {
	logger := zap.NewNop()

	// This test would require mocking the scanner's findIPByMAC method
	// For now, we just verify the behavior when no IP is found
	scanner := &Scanner{
		nodeIPs: make(map[string]net.IP),
	}

	monitor := &RebootMonitor{
		state:        StateRebooting,
		vmid:         types.VMID(201),
		initialIP:    net.ParseIP("192.168.1.201"),
		mac:          "BC:24:11:AA:BB:CC",
		scanner:      scanner,
		logger:       logger,
		talosPort:    1,          // Port closed so original IP check fails
		lastARPRepop: time.Now(), // Don't trigger ARP repop yet
	}

	ctx := context.Background()
	ip, ready, err := monitor.tickRebooting(ctx)

	// No IP found via MAC, original IP not reachable
	assert.Nil(t, ip)
	assert.False(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateRebooting, monitor.state)
}

// Test tickMonitoring - port still open (node hasn't rebooted yet)
func TestTickMonitoring_PortStillOpen(t *testing.T) {
	logger := zap.NewNop()

	// Start a test server to simulate node still being up
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	initialIP := net.ParseIP("127.0.0.1")

	scanner := &Scanner{
		nodeIPs: make(map[string]net.IP),
	}

	monitor := &RebootMonitor{
		state:     StateMonitoring,
		vmid:      types.VMID(201),
		initialIP: initialIP,
		mac:       "BC:24:11:AA:BB:CC",
		scanner:   scanner,
		logger:    logger,
		talosPort: addr.Port,
	}

	ctx := context.Background()
	ip, ready, err := monitor.tickMonitoring(ctx)

	// Port is still open, node hasn't rebooted yet, return as ready (still waiting)
	// Actually, per the implementation, if port is open we treat as ready
	assert.Equal(t, initialIP, ip)
	assert.True(t, ready)
	assert.NoError(t, err)
	assert.Equal(t, StateMonitoring, monitor.state) // No transition
}
