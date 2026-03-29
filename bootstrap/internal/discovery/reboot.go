package discovery

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

// RebootState represents the current phase of the reboot monitoring state machine
type RebootState int

const (
	StateMonitoring RebootState = iota
	StateRebooting
	StateVerifying
)

func (s RebootState) String() string {
	switch s {
	case StateMonitoring:
		return "monitoring"
	case StateRebooting:
		return "rebooting"
	case StateVerifying:
		return "verifying"
	default:
		return "unknown"
	}
}

// RebootMonitor tracks a node through the reboot cycle after config application.
// States: Monitoring (checking if node goes down) -> Rebooting (scanning for new IP) -> Verifying (confirming Talos API up)
type RebootMonitor struct {
	state           RebootState
	vmid            types.VMID
	initialIP       net.IP
	candidateIP     net.IP
	mac             string
	scanner         *Scanner
	logger          *zap.Logger
	lastStateChange time.Time
	lastARPRepop    time.Time
	talosPort       int
	verifyCount     int // consecutive successful port checks in Verifying state
	verifyFailures  int // consecutive failures in Verifying state
}

// NewRebootMonitor creates a monitor for tracking a node through reboot
func NewRebootMonitor(vmid types.VMID, initialIP net.IP, mac string, scanner *Scanner, logger *zap.Logger) *RebootMonitor {
	return &RebootMonitor{
		state:           StateMonitoring,
		vmid:            vmid,
		initialIP:       initialIP,
		mac:             mac,
		scanner:         scanner,
		logger:          logger.With(zap.Int("vmid", int(vmid))),
		lastStateChange: time.Now(),
		talosPort:       50000,
	}
}

// WaitForReady blocks until the node is back online after a reboot, returning the (possibly new) IP.
// maxWait is the total time budget. The monitor polls every 2s, performing ARP repopulation
// every 5s while in the Rebooting state.
func (m *RebootMonitor) WaitForReady(ctx context.Context, maxWait time.Duration) (net.IP, error) {
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timeout waiting for node %d (VMID) after %v in state %s", m.vmid, maxWait, m.state)
			}

			ip, ready, err := m.tick(ctx)
			if err != nil {
				m.logger.Debug("tick error", zap.Error(err), zap.String("state", m.state.String()))
			}
			if ready {
				m.logger.Info("node is ready",
					zap.String("final_ip", ip.String()),
					zap.Bool("ip_changed", !ip.Equal(m.initialIP)))
				return ip, nil
			}
		}
	}
}

func (m *RebootMonitor) tick(ctx context.Context) (net.IP, bool, error) {
	switch m.state {
	case StateMonitoring:
		return m.tickMonitoring(ctx)
	case StateRebooting:
		return m.tickRebooting(ctx)
	case StateVerifying:
		return m.tickVerifying(ctx)
	default:
		return nil, false, fmt.Errorf("unknown state: %d", m.state)
	}
}

// tickMonitoring checks if the node is still reachable on its initial IP.
// If the Talos API port is still up, the node hasn't rebooted yet - return it as ready.
// If unreachable, transition to Rebooting.
func (m *RebootMonitor) tickMonitoring(ctx context.Context) (net.IP, bool, error) {
	if TestPort(m.initialIP.String(), m.talosPort, 2*time.Second) {
		// Node is still up (or came back on same IP), treat as ready
		return m.initialIP, true, nil
	}

	// Connection lost - node is rebooting
	m.logger.Info("node unreachable, entering reboot state",
		zap.String("ip", m.initialIP.String()))
	m.transitionTo(StateRebooting)

	// Kick off immediate ARP repopulation
	if err := m.scanner.RepopulateARP(ctx); err != nil {
		m.logger.Warn("ARP repopulation failed during monitoring", zap.Error(err))
	}
	m.lastARPRepop = time.Now()

	return nil, false, nil
}

// tickRebooting aggressively scans for the node's new IP via MAC address.
// Repopulates ARP tables every 5s and attempts MAC -> IP lookups each tick.
func (m *RebootMonitor) tickRebooting(ctx context.Context) (net.IP, bool, error) {
	// Aggressive ARP repopulation every 5s
	if time.Since(m.lastARPRepop) > 5*time.Second {
		m.logger.Debug("repopulating ARP tables")
		if err := m.scanner.RepopulateARP(ctx); err != nil {
			m.logger.Warn("ARP repopulation failed during rebooting", zap.Error(err))
		}
		m.lastARPRepop = time.Now()
	}

	// Try to rediscover IP by MAC
	newIP, err := m.scanner.findIPByMAC(ctx, m.mac)
	if err == nil && newIP != nil {
		m.candidateIP = newIP
		m.logger.Info("found candidate IP",
			zap.String("candidate_ip", newIP.String()),
			zap.Bool("ip_changed", !newIP.Equal(m.initialIP)))
		m.transitionTo(StateVerifying)
		return nil, false, nil
	}

	// Also check if original IP came back
	if TestPort(m.initialIP.String(), m.talosPort, 2*time.Second) {
		m.candidateIP = m.initialIP
		m.logger.Info("original IP is reachable again")
		m.transitionTo(StateVerifying)
		return nil, false, nil
	}

	return nil, false, nil
}

// tickVerifying confirms the candidate IP has a responding Talos API.
// Requires 2 consecutive successful checks before declaring ready.
// Falls back to Rebooting only after 5 consecutive failures (tolerates DHCP IP
// flapping and port flicker during boot).
func (m *RebootMonitor) tickVerifying(ctx context.Context) (net.IP, bool, error) {
	if !TestPort(m.candidateIP.String(), m.talosPort, 3*time.Second) {
		m.verifyCount = 0
		m.verifyFailures++

		if m.verifyFailures >= 5 {
			m.logger.Warn("candidate IP lost during verification, returning to rebooting",
				zap.String("candidate_ip", m.candidateIP.String()))
			m.transitionTo(StateRebooting)
		}
		return nil, false, nil
	}

	m.verifyFailures = 0
	m.verifyCount++

	if m.verifyCount >= 2 {
		return m.candidateIP, true, nil
	}
	return nil, false, nil
}

func (m *RebootMonitor) transitionTo(newState RebootState) {
	m.logger.Debug("state transition",
		zap.String("from", m.state.String()),
		zap.String("to", newState.String()))
	m.state = newState
	m.lastStateChange = time.Now()
	m.verifyCount = 0
	m.verifyFailures = 0
}

// State returns the current reboot monitor state (for testing)
func (m *RebootMonitor) State() RebootState {
	return m.state
}
