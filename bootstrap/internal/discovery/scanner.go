package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Scanner handles ARP-based IP discovery across Proxmox nodes
type Scanner struct {
	sshUser   string
	sshConfig *ssh.ClientConfig
	nodeIPs   map[string]net.IP

	// Connection pool: one persistent client per Proxmox host IP
	connMu   sync.Mutex
	connPool map[string]*ssh.Client
}

// NewScanner creates a new discovery scanner.
// If insecure SSH is false, host keys are verified against ~/.ssh/known_hosts.
// Pass true and use --insecure-ssh to skip verification entirely.
func NewScanner(sshUser string, nodeIPs map[string]net.IP, insecureSSH bool) *Scanner {
	hostKeyCallback := knownHostsCallback(insecureSSH)
	return &Scanner{
		sshUser: sshUser,
		sshConfig: &ssh.ClientConfig{
			User:            sshUser,
			Auth:            []ssh.AuthMethod{},
			HostKeyCallback: hostKeyCallback,
			Timeout:         10 * time.Second,
		},
		nodeIPs:  nodeIPs,
		connPool: make(map[string]*ssh.Client),
	}
}

// getConn returns a pooled SSH client for the given host IP, dialing on first use.
// The connection is cached for the lifetime of the Scanner.
func (s *Scanner) getConn(ip net.IP) (*ssh.Client, error) {
	addr := fmt.Sprintf("%s:22", ip)

	s.connMu.Lock()
	defer s.connMu.Unlock()

	if c, ok := s.connPool[addr]; ok {
		// Probe with a lightweight no-op to confirm the connection is still alive
		if _, _, err := c.Conn.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return c, nil
		}
		// Stale connection - discard and re-dial below
		c.Close()
		delete(s.connPool, addr)
	}

	c, err := ssh.Dial("tcp", addr, s.sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	s.connPool[addr] = c
	return c, nil
}

// Close releases all pooled SSH connections. Call when the Scanner is done.
func (s *Scanner) Close() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for addr, c := range s.connPool {
		c.Close()
		delete(s.connPool, addr)
	}
}

// SetPrivateKey configures SSH key authentication
func (s *Scanner) SetPrivateKey(keyPath string) error {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	s.sshConfig.Auth = []ssh.AuthMethod{
		ssh.PublicKeys(signer),
	}
	return nil
}

// DiscoverVMs scans all Proxmox nodes for VM configurations and ARP entries
// This replaces your discover_live_state() function
func (s *Scanner) DiscoverVMs(ctx context.Context, vmids []types.VMID) (map[types.VMID]*types.LiveNode, error) {
	results := make(map[types.VMID]*types.LiveNode)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Semaphore to limit concurrency
	sem := make(chan struct{}, 5)

	for _, vmid := range vmids {
		wg.Add(1)
		go func(id types.VMID) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			node, err := s.findVMNode(ctx, id)
			if err != nil {
				// VM might not exist yet, that's ok
				return
			}

			mac, err := s.getVMMAC(ctx, id, node)
			if err != nil {
				return
			}

			ip, err := s.findIPByMAC(ctx, mac)
			if err != nil {
				// IP not found yet, VM might still be booting
				mu.Lock()
				results[id] = &types.LiveNode{
					VMID:   id,
					MAC:    mac,
					Status: types.StatusNotFound,
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			results[id] = &types.LiveNode{
				VMID:         id,
				IP:           ip,
				MAC:          mac,
				Status:       types.StatusDiscovered,
				DiscoveredAt: time.Now(),
			}
			mu.Unlock()
		}(vmid)
	}

	wg.Wait()
	return results, nil
}

// RepopulateARP runs parallel ping sweeps on all Proxmox nodes to refresh
// the ARP cache, ensuring subsequent MAC->IP lookups succeed.
func (s *Scanner) RepopulateARP(ctx context.Context) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(s.nodeIPs))

	for nodeName, nodeIP := range s.nodeIPs {
		wg.Add(1)
		go func(name string, ip net.IP) {
			defer wg.Done()

			if err := s.repopulateNode(ctx, name, ip); err != nil {
				errChan <- fmt.Errorf("node %s: %w", name, err)
			}
		}(nodeName, nodeIP)
	}

	wg.Wait()
	close(errChan)

	// Return first error if any
	for err := range errChan {
		return err
	}
	return nil
}

func (s *Scanner) repopulateNode(_ context.Context, _ string, nodeIP net.IP) error {
	client, err := s.getConn(nodeIP)
	if err != nil {
		return err
	}

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	// Extract subnet from node IP
	ipStr := nodeIP.String()
	lastDot := strings.LastIndex(ipStr, ".")
	if lastDot == -1 {
		return fmt.Errorf("invalid IP format")
	}
	subnet := ipStr[:lastDot]

	// Flush ARP and ping sweep subnet
	cmd := fmt.Sprintf("ip -s -s neigh flush all && seq 1 254 | xargs -P 100 -I{} ping -c 1 -W 1 %s.{} >/dev/null 2>&1 || true", subnet)

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("ARP repop command: %w", err)
	}

	return nil
}

// findIPByMAC scans ARP tables across all nodes for a MAC address
func (s *Scanner) findIPByMAC(ctx context.Context, mac string) (net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	results := make(chan net.IP, len(s.nodeIPs))
	var wg sync.WaitGroup

	for nodeName, nodeIP := range s.nodeIPs {
		wg.Add(1)
		go func(name string, ip net.IP) {
			defer wg.Done()

			client, err := s.getConn(ip)
			if err != nil {
				return
			}

			session, err := client.NewSession()
			if err != nil {
				return
			}
			defer session.Close()

			output, err := session.Output("cat /proc/net/arp")
			if err != nil {
				return
			}

			foundIP := parseARPTable(string(output), mac)
			if foundIP != nil {
				select {
				case results <- foundIP:
				case <-ctx.Done():
				}
			}
		}(nodeName, nodeIP)
	}

	// Close results when all goroutines done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Return first result
	select {
	case ip := <-results:
		return ip, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// parseARPTable extracts IP for given MAC from /proc/net/arp output
func parseARPTable(output, targetMAC string) net.IP {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		ip := fields[0]
		mac := strings.ToUpper(fields[3])

		// Skip incomplete entries
		if mac == "00:00:00:00:00:00" || mac == "INCOMPLETE" {
			continue
		}

		if mac == strings.ToUpper(targetMAC) {
			return net.ParseIP(ip)
		}
	}
	return nil
}

// findVMNode determines which Proxmox node hosts a VM
func (s *Scanner) findVMNode(_ context.Context, vmid types.VMID) (string, error) {
	for nodeName, nodeIP := range s.nodeIPs {
		found, err := s.checkVMOnNode(vmid, nodeIP)
		if err != nil {
			continue
		}

		if found {
			return nodeName, nil
		}
	}

	return "", fmt.Errorf("VM %d not found on any node", vmid)
}

func (s *Scanner) checkVMOnNode(vmid types.VMID, nodeIP net.IP) (bool, error) {
	client, err := s.getConn(nodeIP)
	if err != nil {
		return false, err
	}

	session, err := client.NewSession()
	if err != nil {
		return false, err
	}
	defer session.Close()

	if err := session.Run(fmt.Sprintf("qm status %d", vmid)); err != nil {
		return false, err
	}
	return true, nil
}

// getVMMAC extracts MAC address from VM config
func (s *Scanner) getVMMAC(_ context.Context, vmid types.VMID, node string) (string, error) {
	nodeIP, ok := s.nodeIPs[node]
	if !ok {
		return "", fmt.Errorf("unknown node: %s", node)
	}

	client, err := s.getConn(nodeIP)
	if err != nil {
		return "", err
	}

	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	output, err := session.Output(fmt.Sprintf("qm config %d", vmid))
	if err != nil {
		return "", err
	}

	// Extract MAC from net0: virtio=XX:XX:XX:XX:XX:XX,bridge=vmbr0
	re := regexp.MustCompile(`net\d+:.*virtio=([0-9A-Fa-f:]+)`)
	matches := re.FindStringSubmatch(string(output))
	if len(matches) < 2 {
		return "", fmt.Errorf("no MAC found in VM config")
	}

	return strings.ToUpper(matches[1]), nil
}

// DiscoverProxmoxNodes queries a known seed Proxmox node via SSH to get the full
// cluster node list using `pvesh get /nodes --output-format json`. This replaces
// the static node IP map hardcoded in types.DefaultConfig.
//
// seedIP is any Proxmox node in the cluster (already known). The returned map
// maps node name -> management IP (from the `ip` field in pvesh output).
func DiscoverProxmoxNodes(sshUser, keyPath string, seedIP net.IP, insecureSSH bool) (map[string]net.IP, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: knownHostsCallback(insecureSSH),
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", seedIP), cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial seed node: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	out, err := session.Output("pvesh get /nodes --output-format json 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("pvesh get nodes output: %w", err)
	}

	return parseProxmoxNodes(string(out))
}

// parseProxmoxNodes parses the JSON output of `pvesh get /nodes`.
// Each entry looks like: {"node":"pve1","ip":"192.168.100",...}
func parseProxmoxNodes(jsonOut string) (map[string]net.IP, error) {
	// Minimal JSON unmarshal without importing encoding/json - use regex for simplicity
	// since pvesh output is well-structured and we only need node+ip fields.
	nodeRe := regexp.MustCompile(`"node"\s*:\s*"([^"]+)"`)
	ipRe := regexp.MustCompile(`"ip"\s*:\s*"([^"]+)"`)

	nodeMatches := nodeRe.FindAllStringSubmatch(jsonOut, -1)
	ipMatches := ipRe.FindAllStringSubmatch(jsonOut, -1)

	if len(nodeMatches) == 0 {
		return nil, fmt.Errorf("no nodes found in pvesh output")
	}
	if len(nodeMatches) != len(ipMatches) {
		return nil, fmt.Errorf("node/ip count mismatch in pvesh output (%d vs %d)", len(nodeMatches), len(ipMatches))
	}

	result := make(map[string]net.IP, len(nodeMatches))
	for i, nm := range nodeMatches {
		nodeName := nm[1]
		ipStr := ipMatches[i][1]
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP %q for node %q", ipStr, nodeName)
		}
		result[nodeName] = ip
	}
	return result, nil
}

// RefreshProxmoxNodes queries all known Proxmox nodes via SSH to discover the
// full cluster node list using `pvesh get /nodes --output-format json`, AND
// updates s.nodeIPs with any newly discovered nodes. Ues the connection pool.
// Falls back silently - if all nodes fail the existing static map is kept.
func (s *Scanner) RefreshProxmoxNodes(ctx context.Context) {
	for _, nodeIP := range s.nodeIPs {
		client, err := s.getConn(nodeIP)
		if err != nil {
			continue
		}

		session, err := client.NewSession()
		if err != nil {
			continue
		}

		out, err := session.Output("pvesh get /nodes --output-format json 2>/dev/null")
		session.Close()
		if err != nil || len(out) == 0 {
			continue
		}

		discovered, err := parseProxmoxNodes(string(out))
		if err != nil || len(discovered) == 0 {
			continue
		}

		s.connMu.Lock()
		for name, ip := range discovered {
			s.nodeIPs[name] = ip
		}
		s.connMu.Unlock()
		return
	}
}

// MarkJoinedNodes sets Status=StatusJoined on any LiveNode whose IP is in
// the given set of cluster member IPs/hostnames.
func (s *Scanner) MarkJoinedNodes(memberAddrs []string, results map[types.VMID]*types.LiveNode) {
	memberSet := make(map[string]bool, len(memberAddrs))
	for _, m := range memberAddrs {
		memberSet[m] = true
	}
	for _, node := range results {
		if node.IP != nil && memberSet[node.IP.String()] {
			node.Status = types.StatusJoined
		}
	}
}

// knownHostsCallback returns an ssh.HostKeyCallback. When insecure is true,
// all host keys are accepted. Otherwise, keys are verified against the user's
// ~/.ssh/known_hosts file. If that file is missing or unredable, connections
// are rejected with a hint to either populate known_hosts or use --insecure-ssh
func knownHostsCallback(insecure bool) ssh.HostKeyCallback {
	if insecure {
		return ssh.InsecureIgnoreHostKey()
	}

	home, err := os.UserHomeDir()
	if err == nil {
		khPath := filepath.Join(home, ".ssh", "known_hosts")
		if cb, err := knownhosts.New(khPath); err == nil {
			return cb
		}
	}

	// Fallback: reject with actionable message
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		return fmt.Errorf("SSh host key verification failed for %s: add host keys with ssh-keyscan or use --insecure-ssh", hostname)
	}
}

// StopVM sends a graceful shutdown command to a Proxmox VM via SSH.
func (s *Scanner) StopVM(ctx context.Context, vmid int, nodeIP net.IP) error {
	client, err := s.getConn(nodeIP)
	if err != nil {
		return fmt.Errorf("SSH connect to %s: %w", nodeIP, err)
	}

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session to %s: %w", nodeIP, err)
	}
	defer session.Close()

	cmd := fmt.Sprintf("qm stop %d", vmid)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("qm stop %d: %w", vmid, err)
	}
	return nil
}

// TestPort checks if a port is open on an IP
func TestPort(ip string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
