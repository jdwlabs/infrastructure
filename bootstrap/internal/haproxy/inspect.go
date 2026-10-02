package haproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

// ConfigPath is where the generator's rendered config is installed.
const ConfigPath = "/etc/haproxy/haproxy.cfg"

// statsSocket is the admin socket the generated config declares in its global
// section. Reading it is the only way to see per-backend health without
// scraping the HTTP stats page, which sits behind the credentials the config
// itself carries.
const statsSocket = "/run/haproxy/admin.sock"

// showStatCmd asks the runtime API for the stats CSV. socat is the documented
// client; nc's Unix-socket mode is tried second so a host that predates the
// cloud-init package list still answers instead of reporting no backends.
const showStatCmd = `echo "show stat" | sudo socat -T2 stdio ` + statsSocket +
	` 2>/dev/null || echo "show stat" | sudo nc -q2 -U ` + statsSocket

// systemd's own vocabulary for `is-active`. Anything else on that stream is
// some other failure talking — a missing sudo, a broken pipe — and echoing it
// back as the service state would invent a state that does not exist.
var systemdStates = map[string]bool{
	"active": true, "reloading": true, "inactive": true, "failed": true,
	"activating": true, "deactivating": true, "unknown": true,
}

// ServiceState returns the systemd activation state of the haproxy unit.
//
// A unit that is not running is a fact to report, not a failure to inspect:
// `is-active` exits 3 for an inactive unit, so the exit status is deliberately
// not what decides success here.
func (c *Client) ServiceState(_ context.Context) (string, error) {
	return c.unitState("haproxy")
}

// KeepalivedState returns the systemd activation state of the keepalived unit.
// An instance where it is not active cannot take the virtual address over, so
// a group with one such member is not redundant however healthy it looks.
func (c *Client) KeepalivedState(_ context.Context) (string, error) {
	return c.unitState("keepalived")
}

func (c *Client) unitState(unit string) (string, error) {
	out, err := c.runner.runSSHOutput("systemctl is-active " + unit)
	state := strings.TrimSpace(lastLine(out))
	if systemdStates[state] {
		return state, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s service state: %w", unit, err)
	}
	return "", fmt.Errorf("read %s service state: unexpected answer %q", unit, state)
}

// HoldsAddress reports whether addr is configured on the host right now. Asked
// of each instance directly rather than inferred from which one answers on the
// virtual address: that would name one holder even when two instances both
// believe they hold it.
func (c *Client) HoldsAddress(_ context.Context, addr net.IP) (bool, error) {
	out, err := c.runner.runSSHOutput("ip -o -4 addr show")
	if err != nil {
		return false, fmt.Errorf("read interface addresses: %w", err)
	}
	if !strings.Contains(out, "inet ") {
		return false, fmt.Errorf("read interface addresses: unexpected answer %q", strings.TrimSpace(out))
	}
	return strings.Contains(out, "inet "+addr.String()+"/"), nil
}

// Stats reads per-server health from the runtime stats socket.
func (c *Client) Stats(_ context.Context) ([]ServerStat, error) {
	out, err := c.runner.runSSHOutput(showStatCmd)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("read stats socket: %w", err)
	}
	return ParseStats(out)
}

// DeployedConfig reads the config file currently installed on the host.
func (c *Client) DeployedConfig(_ context.Context) (string, error) {
	out, err := c.runner.runSSHOutput("sudo cat " + ConfigPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ConfigPath, err)
	}
	return out, nil
}

// Hash fingerprints a config for the deployed-hash record kept in cluster
// state. Line endings are normalised first: the config is written from Go and
// read back through a shell, and a CRLF round-trip would otherwise report drift
// against a byte-identical file.
func Hash(config string) string {
	sum := sha256.Sum256([]byte(normalizeConfig(config)))
	return hex.EncodeToString(sum[:])
}

func normalizeConfig(config string) string {
	return strings.TrimRight(strings.ReplaceAll(config, "\r\n", "\n"), "\n") + "\n"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")), "\n")
	return lines[len(lines)-1]
}
