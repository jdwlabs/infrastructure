package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/talos"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

// Manager handles the three-way state reconciliation
// (Terraform desired → Local deployed → Live reality)
type Manager struct {
	mu       sync.Mutex // protects ClusterState mutations (UpdateNodeState, RemoveNodeState)
	config   *types.Config
	stateDir string
	nodesDir string
	logger   *zap.Logger
}

// NewManager creates a new state manager
func NewManager(cfg *types.Config, logger *zap.Logger) *Manager {
	clusterDir := filepath.Join("clusters", cfg.ClusterName)
	return &Manager{
		config:   cfg,
		stateDir: filepath.Join(clusterDir, "state"),
		nodesDir: filepath.Join(clusterDir, "nodes"),
		logger:   logger,
	}
}

// NodeConfigPath returns the path to a node's config file
func (m *Manager) NodeConfigPath(vmid types.VMID, role types.Role) string {
	return filepath.Join(m.nodesDir, fmt.Sprintf("node-%s-%d.yaml", role, vmid))
}

// LoadDesiredState parses terraform.tfvars into NodeSpecs
// This replaces your parse_terraform_array() function with proper HCL parsing
func (m *Manager) LoadDesiredState(_ context.Context) (map[types.VMID]*types.NodeSpec, error) {
	raw, err := os.ReadFile(m.config.TerraformTFVars)
	if err != nil {
		return nil, fmt.Errorf("read terraform.tfvars: %w", err)
	}

	// Normalize Windows line endings and strip UTF-8 BOM
	data := []byte(strings.TrimPrefix(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\xef\xbb\xbf"))

	// Parse HCL properly instead of fragile regex
	var tfConfig struct {
		TalosControlConfiguration []struct {
			VMID   int    `hcl:"vmid"`
			Name   string `hcl:"vm_name"`
			Node   string `hcl:"node_name"`
			CPU    int    `hcl:"cpu_cores"`
			Memory int    `hcl:"memory"`
			Disk   int    `hcl:"disk_size"`
		} `hcl:"talos_control_configuration,block"`
		TalosWorkerConfiguration []struct {
			VMID   int    `hcl:"vmid"`
			Name   string `hcl:"vm_name"`
			Node   string `hcl:"node_name"`
			CPU    int    `hcl:"cpu_cores"`
			Memory int    `hcl:"memory"`
			Disk   int    `hcl:"disk_size"`
		} `hcl:"talos_worker_configuration,block"`
	}

	// Use HCL parser
	ctxHCL := &hcl.EvalContext{}
	err = hclsimple.Decode("terraform.tfvars", data, ctxHCL, &tfConfig)
	if err != nil {
		// Fallback: try manual parsing for simple cases
		return m.fallbackParseTerraform(data)
	}

	specs := make(map[types.VMID]*types.NodeSpec)

	// Process control planes
	for _, cfg := range tfConfig.TalosControlConfiguration {
		vmid := types.VMID(cfg.VMID)
		specs[vmid] = &types.NodeSpec{
			VMID:   vmid,
			Name:   cfg.Name,
			Node:   cfg.Node,
			CPU:    cfg.CPU,
			Memory: cfg.Memory,
			Disk:   cfg.Disk,
			Role:   types.RoleControlPlane,
		}
	}

	// Process workers
	for _, cfg := range tfConfig.TalosWorkerConfiguration {
		vmid := types.VMID(cfg.VMID)
		specs[vmid] = &types.NodeSpec{
			VMID:   vmid,
			Name:   cfg.Name,
			Node:   cfg.Node,
			CPU:    cfg.CPU,
			Memory: cfg.Memory,
			Disk:   cfg.Disk,
			Role:   types.RoleWorker,
		}
	}

	return specs, nil
}

// fallbackParseTerraform handles terraform.tfvars parsing when HCL library fails.
// Uses a brace-counting state machine similar to the bash parse_terraform_array
func (m *Manager) fallbackParseTerraform(data []byte) (map[types.VMID]*types.NodeSpec, error) {
	specs := make(map[types.VMID]*types.NodeSpec)

	content := string(data)

	// Parse both configuration arrays
	cpEntries := parseArrayBlocks(content, "talos_control_configuration")
	for _, entry := range cpEntries {
		spec := parseBlockFields(entry, types.RoleControlPlane)
		if spec != nil {
			specs[spec.VMID] = spec
		}
	}

	wEntries := parseArrayBlocks(content, "talos_worker_configuration")
	for _, entry := range wEntries {
		spec := parseBlockFields(entry, types.RoleWorker)
		if spec != nil {
			specs[spec.VMID] = spec
		}
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("fallback parser found no nodes in terraform.tfvars")
	}

	return specs, nil
}

// parseArrayBlocks extracts individual block strings from a terraform array variable.
// e.g., talos_control_configuration = [ { ... }, { ... } ]
func parseArrayBlocks(content, varName string) []string {
	var blocks []string

	// Find the variable assignment
	idx := strings.Index(content, varName)
	if idx == -1 {
		return blocks
	}

	// Find the opening bracket
	rest := content[idx:]
	bracketIdx := strings.Index(rest, "[")
	if bracketIdx == -1 {
		return blocks
	}
	rest = rest[bracketIdx+1:]

	// Use brace counting to extract individual blocks
	braceDepth := 0
	blockStart := -1

	for i, ch := range rest {
		switch ch {
		case '{':
			if braceDepth == 0 {
				blockStart = i
			}
			braceDepth++
		case '}':
			braceDepth--
			if braceDepth == 0 && blockStart >= 0 {
				blocks = append(blocks, rest[blockStart:i+1])
				blockStart = -1
			}
		case ']':
			if braceDepth == 0 {
				return blocks
			}
		}
	}

	return blocks
}

// parseBlockFields extracts node spec fields from a single terraform block string
func parseBlockFields(block string, role types.Role) *types.NodeSpec {
	spec := &types.NodeSpec{
		Role:   role,
		Node:   "pve1",
		CPU:    4,
		Memory: 4096,
		Disk:   100,
	}

	spec.VMID = types.VMID(extractIntField(block, "vmid"))
	if spec.VMID == 0 {
		return nil
	}

	if name := extractStringField(block, "vm_name"); name != "" {
		spec.Name = name
	}
	if node := extractStringField(block, "node_name"); node != "" {
		spec.Node = node
	}
	if cpu := extractIntField(block, "cpu_cores"); cpu > 0 {
		spec.CPU = cpu
	}
	if mem := extractIntField(block, "memory"); mem > 0 {
		spec.Memory = mem
	}
	if disk := extractIntField(block, "disk_size"); disk > 0 {
		spec.Disk = disk
	}

	return spec
}

// extractStringField extracts a string value for a key from a terraform block
func extractStringField(block, key string) string {
	re := regexp.MustCompile(key + `\s*=\s*"([^"]*)"`)
	matches := re.FindStringSubmatch(block)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// extractIntField extracts an integer value for a key from a terraform block.
// Handles both quoted ("201") and unquoted (201) integer formats.
func extractIntField(block, key string) int {
	// Try unquoted first (e.g. vmid = 201), then quoted (e.g., vmid = "201")
	re := regexp.MustCompile(key + `\s*=\s*"?(\d+)"?`)
	matches := re.FindStringSubmatch(block)
	if len(matches) >= 2 {
		val, err := strconv.Atoi(matches[1])
		if err == nil {
			return val
		}
	}
	return 0
}

// ComputeTerraformHash calculates SHA256 of terraform.tfvars
func (m *Manager) ComputeTerraformHash() (string, error) {
	data, err := os.ReadFile(m.config.TerraformTFVars)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// LoadDeployedState reads bootstrap-state.json
func (m *Manager) LoadDeployedState(_ context.Context) (*types.ClusterState, error) {
	stateFile := filepath.Join(m.stateDir, "bootstrap-state.json")

	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		// Fresh start - return empty state
		return &types.ClusterState{
			ClusterName:          m.config.ClusterName,
			ControlPlaneEndpoint: m.config.ControlPlaneEndpoint,
			HAProxyIP:            m.config.HAProxyIP,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	var state types.ClusterState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state file (corrupted?): %w", err)
	}

	// Migrate bash-format state: if data was wrapped in a "deployed_sate" key,
	if !state.BootstrapCompleted && len(state.ControlPlanes) == 0 && len(state.Workers) == 0 {
		var wrapper struct {
			DeployedState types.ClusterState `json:"deployed_state"`
		}
		if err := json.Unmarshal(data, &wrapper); err == nil && (wrapper.DeployedState.BootstrapCompleted || len(wrapper.DeployedState.ControlPlanes) == 0 || len(wrapper.DeployedState.Workers) > 0) {
			state = wrapper.DeployedState
		}
	}

	// Backfill cluster metadata from config if missing (e.g. migrated state)
	if state.ClusterName == "" {
		state.ClusterName = m.config.ClusterName
	}
	if state.ControlPlaneEndpoint == "" {
		state.ControlPlaneEndpoint = m.config.ControlPlaneEndpoint
	}
	if state.HAProxyIP == nil {
		state.HAProxyIP = m.config.HAProxyIP
	}

	return &state, nil
}

// BuildReconcilePlan computes the diff between desired, deployed, and live
// This replaces your build_reconcile_plan() with proper logic
func (m *Manager) BuildReconcilePlan(
	_ context.Context,
	desired map[types.VMID]*types.NodeSpec,
	deployed *types.ClusterState,
	live map[types.VMID]*types.LiveNode,
) (*types.ReconcilePlan, error) {

	plan := &types.ReconcilePlan{}

	// Build lookup maps for O(1) access (vs bash array iteration)
	deployedCPs := make(map[types.VMID]types.NodeState)
	for _, cp := range deployed.ControlPlanes {
		deployedCPs[cp.VMID] = cp
	}

	deployedWorkers := make(map[types.VMID]types.NodeState)
	for _, w := range deployed.Workers {
		deployedWorkers[w.VMID] = w
	}

	// Check for additions
	for vmid, spec := range desired {
		switch spec.Role {
		case types.RoleControlPlane:
			if _, exists := deployedCPs[vmid]; !exists {
				plan.AddControlPlanes = append(plan.AddControlPlanes, vmid)
			}
		case types.RoleWorker:
			if _, exists := deployedWorkers[vmid]; !exists {
				plan.AddWorkers = append(plan.AddWorkers, vmid)
			}
		}
	}

	// Check for removals and role changes
	for _, cp := range deployed.ControlPlanes {
		spec, exists := desired[cp.VMID]
		if !exists {
			plan.RemoveControlPlanes = append(plan.RemoveControlPlanes, cp.VMID)
		} else if spec.Role == types.RoleWorker {
			// Role change: CP -> Worker
			plan.RemoveControlPlanes = append(plan.RemoveControlPlanes, cp.VMID)
			plan.AddWorkers = append(plan.AddWorkers, cp.VMID)
		}
	}

	for _, w := range deployed.Workers {
		spec, exists := desired[w.VMID]
		if !exists {
			plan.RemoveWorkers = append(plan.RemoveWorkers, w.VMID)
		} else if spec.Role == types.RoleControlPlane {
			// Role change: Worker -> CP
			plan.RemoveWorkers = append(plan.RemoveWorkers, w.VMID)
			plan.AddControlPlanes = append(plan.AddControlPlanes, w.VMID)
		}
	}

	// Build set of nodes being added to skip them in drift check
	adding := make(map[types.VMID]bool)
	for _, vmid := range plan.AddControlPlanes {
		adding[vmid] = true
	}
	for _, vmid := range plan.AddWorkers {
		adding[vmid] = true
	}

	// Check for config drift (hash comparison), skip nodes being freshly added
	for vmid, spec := range desired {
		if adding[vmid] {
			continue
		}
		configFile := m.NodeConfigPath(vmid, spec.Role)
		currentHash, err := talos.HashFile(configFile)
		if err != nil {
			// Config doesn't exist, needs generation
			plan.UpdateConfigs = append(plan.UpdateConfigs, vmid)
			continue
		}

		var deployedHash string
		switch spec.Role {
		case types.RoleControlPlane:
			if cp, ok := deployedCPs[vmid]; ok {
				deployedHash = cp.ConfigHash
			}
		case types.RoleWorker:
			if w, ok := deployedWorkers[vmid]; ok {
				deployedHash = w.ConfigHash
			}
		}

		if currentHash != deployedHash || m.config.ForceReconfigure {
			plan.UpdateConfigs = append(plan.UpdateConfigs, vmid)
		} else {
			plan.NoOp = append(plan.NoOp, vmid)
		}
	}

	// Check if bootstrap needed:
	// - Deployed CPs exist but bootstrap not completed (interrupted bootstrap)
	// - No deployed CPs but we're about to add some (fresh cluster)
	if !deployed.BootstrapCompleted {
		if len(deployed.ControlPlanes) > 0 || len(plan.AddControlPlanes) > 0 {
			plan.NeedsBootstrap = true
		}
	}

	// Use live discovery data for IP synchronization and unreachable node warnings
	for vmid, liveNode := range live {
		if liveNode.IP != nil && liveNode.Status == types.StatusDiscovered {
			// Sync discovered IP into deployed state if it has changed
			if cp, ok := deployedCPs[vmid]; ok {
				if cp.IP == nil || !cp.IP.Equal(liveNode.IP) {
					if m.logger != nil {
						oldIP := "<nil>"
						if cp.IP != nil {
							oldIP = cp.IP.String()
						}
						m.logger.Info("IP changed for deployed CP (live sync)",
							zap.Int("vmid", int(vmid)),
							zap.String("old_ip", oldIP),
							zap.String("new_ip", liveNode.IP.String()))
					}
					cp.IP = liveNode.IP
					deployedCPs[vmid] = cp
					// Update in actual deployed state
					for i := range deployed.ControlPlanes {
						if deployed.ControlPlanes[i].VMID == vmid {
							deployed.ControlPlanes[i].IP = liveNode.IP
							break
						}
					}
				}
			}
			if w, ok := deployedWorkers[vmid]; ok {
				if w.IP == nil || !w.IP.Equal(liveNode.IP) {
					if m.logger != nil {
						oldIP := "<nil>"
						if w.IP != nil {
							oldIP = w.IP.String()
						}
						m.logger.Info("IP changed for deployed worker (live sync)",
							zap.Int("vmid", int(vmid)),
							zap.String("old_ip", oldIP),
							zap.String("new_ip", liveNode.IP.String()))
					}
					w.IP = liveNode.IP
					deployedWorkers[vmid] = w
					for i := range deployed.Workers {
						if deployed.Workers[i].VMID == vmid {
							deployed.Workers[i].IP = liveNode.IP
							break
						}
					}
				}
			}
		} else if liveNode.Status == types.StatusNotFound {
			// Warn about deployed nodes that are unreachable
			if _, ok := deployedCPs[vmid]; ok {
				if m.logger != nil {
					m.logger.Warn("deployed control plane not reachable in live discovery",
						zap.Int("vmid", int(vmid)))
				}
			}
			if _, ok := deployedWorkers[vmid]; ok {
				if m.logger != nil {
					m.logger.Warn("deployed worker not reachable in live discovery",
						zap.Int("vmid", int(vmid)))
				}
			}
		}
	}

	// Warn about even control plane counts (etcd split-brain risk)
	finalCPCount := len(deployed.ControlPlanes) + len(plan.AddControlPlanes) - len(plan.RemoveControlPlanes)
	if finalCPCount > 0 && finalCPCount%2 == 0 && m.logger != nil {
		m.logger.Warn("event number of control planes detected - odd count recommended for etcd quorum",
			zap.Int("final_cp_count", finalCPCount))
	}

	// Sort for deterministic output
	sort.Slice(plan.AddControlPlanes, func(i, j int) bool { return plan.AddControlPlanes[i] < plan.AddControlPlanes[j] })
	sort.Slice(plan.AddWorkers, func(i, j int) bool { return plan.AddWorkers[i] < plan.AddWorkers[j] })
	sort.Slice(plan.RemoveControlPlanes, func(i, j int) bool { return plan.RemoveControlPlanes[i] < plan.RemoveControlPlanes[j] })
	sort.Slice(plan.RemoveWorkers, func(i, j int) bool { return plan.RemoveWorkers[i] < plan.RemoveWorkers[j] })
	sort.Slice(plan.UpdateConfigs, func(i, j int) bool { return plan.UpdateConfigs[i] < plan.UpdateConfigs[j] })

	return plan, nil
}

// Save persists state to disk atomically
func (m *Manager) Save(_ context.Context, state *types.ClusterState) error {
	if err := os.MkdirAll(m.stateDir, 0700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	state.Timestamp = time.Now()

	hash, err := m.ComputeTerraformHash()
	if err != nil {
		return fmt.Errorf("compute terraform hash: %w", err)
	}
	state.TerraformHash = hash

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	stateFile := filepath.Join(m.stateDir, "bootstrap-state.json")

	// Atomic write: write to temp, then rename
	tempFile := stateFile + ".tmp"
	if err := os.WriteFile(tempFile, data, 0600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}

	// On Windows, os.Rename cannot overwrite an existing file
	if runtime.GOOS == "windows" {
		_ = os.Remove(stateFile)
	}
	if err := os.Rename(tempFile, stateFile); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}

	return nil
}

// UpdateNodeState updates a node's state in the cluster state.
// Safe for concurrent use from multiple goroutines.
func (m *Manager) UpdateNodeState(state *types.ClusterState, vmid types.VMID, ip string, hash string, role types.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeState := types.NodeState{
		VMID:       vmid,
		ConfigHash: hash,
		LastSeen:   time.Now(),
		Role:       role,
	}
	if ip != "" {
		nodeState.IP = parseIP(ip)
	}

	switch role {
	case types.RoleControlPlane:
		// Check if already exists
		found := false
		for i, cp := range state.ControlPlanes {
			if cp.VMID == vmid {
				state.ControlPlanes[i] = nodeState
				found = true
				break
			}
		}
		if !found {
			state.ControlPlanes = append(state.ControlPlanes, nodeState)
		}
	case types.RoleWorker:
		found := false
		for i, w := range state.Workers {
			if w.VMID == vmid {
				state.Workers[i] = nodeState
				found = true
				break
			}
		}
		if !found {
			state.Workers = append(state.Workers, nodeState)
		}
	}
}

// RemoveNodeState removes a node from the active cluster state and moves it
// to the RemovedNodes audit trail. Safe for concurrent use from multiple goroutines.
func (m *Manager) RemoveNodeState(state *types.ClusterState, vmid types.VMID, role types.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	switch role {
	case types.RoleControlPlane:
		filtered := make([]types.NodeState, 0, len(state.ControlPlanes))
		for _, cp := range state.ControlPlanes {
			if cp.VMID == vmid {
				cp.Role = role
				cp.RemovedAt = &now
				state.RemovedNodes = append(state.RemovedNodes, cp)
			} else {
				filtered = append(filtered, cp)
			}
		}
		state.ControlPlanes = filtered

	case types.RoleWorker:
		filtered := make([]types.NodeState, 0, len(state.Workers))
		for _, w := range state.Workers {
			if w.VMID == vmid {
				w.Role = role
				w.RemovedAt = &now
				state.RemovedNodes = append(state.RemovedNodes, w)
			} else {
				filtered = append(filtered, w)
			}
		}
		state.Workers = filtered
	}
}

func parseIP(s string) net.IP {
	return net.ParseIP(s)
}

// ResolveTFVarsPath locates the terraform.tfvars file. Searches:
//  1. The configured path (relative to cwd)
//  2. Inside the terraform/ subdirectory (repo root invocation)
//  3. Parent directory (Go binary is in talos/cluster/go/, tfvars in talos/cluster/)
//  4. Next to the binary itself
func (m *Manager) ResolveTFVarsPath() error {
	base := filepath.Base(m.config.TerraformTFVars)
	candidates := []string{
		m.config.TerraformTFVars,
		filepath.Join("terraform", base),
		filepath.Join("..", base),
	}
	// Also try the directory containing the binary
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", base))
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			if candidate != m.config.TerraformTFVars {
				m.logger.Info("resolved tfvars", zap.String("path", candidate))
				m.config.TerraformTFVars = candidate
			}
			return nil
		}
	}

	abs, _ := filepath.Abs(m.config.TerraformTFVars)
	return fmt.Errorf("terraform.tfvars not found (searched: %v, absolute: %s)", candidates, abs)
}

// LoadTerraformExtras parses additional fields from terraform.tfvars that aren't
// part of the node configuration arrays. Only updates fields still at their
// zero/empty values so CLI flags and envs take precedence.
func (m *Manager) LoadTerraformExtras(_ context.Context) error {
	data, err := os.ReadFile(m.config.TerraformTFVars)
	if err != nil {
		return fmt.Errorf("read terraform.tfvars: %w", err)
	}

	// Normalize Windows line endings and strip UTF-8 BOM
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")

	// Only update cluster name if it's still at the default value
	if m.config.ClusterName == "cluster" {
		if name := extractSimpleStringField(content, "cluster_name"); name != "" {
			m.config.ClusterName = name
			// Re-derive paths based on new cluster name
			clusterDir := filepath.Join("clusters", m.config.ClusterName)
			m.config.SecretsDir = filepath.Join(clusterDir, "secrets")
			m.stateDir = filepath.Join(clusterDir, "state")
			m.nodesDir = filepath.Join(clusterDir, "nodes")
		}
	}

	// Proxmox credentials
	if m.config.ProxmoxTokenID == "" {
		if v := extractSimpleStringField(content, "proxmox_api_token_id"); v != "" {
			m.config.ProxmoxTokenID = v
		}
	}
	if m.config.ProxmoxTokenSecret == "" {
		if v := extractSimpleStringField(content, "proxmox_api_token_secret"); v != "" {
			m.config.ProxmoxTokenSecret = v
		}
	}

	// Proxmox SSH host - extract IP from the full API URL
	if m.config.ProxmoxSSHHost == "" {
		if v := extractSimpleStringField(content, "proxmox_endpoint"); v != "" {
			m.config.ProxmoxSSHHost = extractURLHost(v)
		}
	}

	// Control plane endpoint
	if m.config.ControlPlaneEndpoint == "" {
		if v := extractSimpleStringField(content, "control_plane_endpoint"); v != "" {
			m.config.ControlPlaneEndpoint = v
		}
	}

	// HAProxy IP
	if m.config.HAProxyIP == nil {
		if v := extractSimpleStringField(content, "haproxy_ip"); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				m.config.HAProxyIP = ip
			}
		}
	}

	// HAProxy SSH key
	if m.config.HAProxySSHKeyPath == "" {
		if v := extractSimpleStringField(content, "haproxy_ssh_key_path"); v != "" {
			m.config.HAProxySSHKeyPath = v
		}
	}

	// HAProxy credentials
	if m.config.HAProxyLoginUser == "" {
		if v := extractSimpleStringField(content, "haproxy_login_user"); v != "" {
			m.config.HAProxyLoginUser = v
		}
	}
	if m.config.HAProxyStatsUser == "" {
		if v := extractSimpleStringField(content, "haproxy_stats_user"); v != "" {
			m.config.HAProxyStatsUser = v
		}
	}
	if m.config.HAProxyStatsPassword == "" {
		if v := extractSimpleStringField(content, "haproxy_stats_password"); v != "" {
			m.config.HAProxyStatsPassword = v
		}
	}

	// Kubernetes and Talos versions
	if m.config.KubernetesVersion == "" {
		if v := extractSimpleStringField(content, "kubernetes_version"); v != "" {
			m.config.KubernetesVersion = v
		}
	}
	if m.config.TalosVersion == "" {
		if v := extractSimpleStringField(content, "talos_version"); v != "" {
			m.config.TalosVersion = v
		}
	}
	if m.config.InstallerImage == "" {
		if v := extractSimpleStringField(content, "installer_image"); v != "" {
			m.config.InstallerImage = v
		}
	}

	// Proxmox node IPs map
	if len(m.config.ProxmoxNodeIPs) == 0 {
		rawMap := parseTFVarsMap(content, "proxmox_node_ips")
		if len(rawMap) > 0 {
			nodeIPs := make(map[string]net.IP, len(rawMap))
			for nodeName, ipStr := range rawMap {
				if ip := net.ParseIP(ipStr); ip != nil {
					nodeIPs[nodeName] = ip
				} else {
					m.logger.Warn("invalid IP in proxmox_node_ips",
						zap.String("node", nodeName), zap.String("value", ipStr))
				}
			}
			if len(nodeIPs) > 0 {
				m.config.ProxmoxNodeIPs = nodeIPs
			}
		}
	}

	return nil
}

// extractSimpleStringField extracts a top-level string assignment from HCL/tfvars content.
// Matches patterns like: key = "value" (with optional leading whitespace)
func extractSimpleStringField(content, key string) string {
	re := regexp.MustCompile(`(?m)^\s*` + key + `\s*=\s*"([^"]*)"`)
	matches := re.FindStringSubmatch(content)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// extractURLHost parses a URL and returns just the hostname (no scheme, port, or path).
// e.g., "https://192.168.1.200:8006/api2/json" -> "192.168.1.200"
// Falls back to returning rawURL unchanged if parsing fails.
func extractURLHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return rawURL
	}
	return u.Hostname()
}

// parseTFVarsMap extracts a string->string map from an HCL map literal.
// Matches: key = { k1 = "v1", k2 = "v2" }
// Returns an empty map if the key is not found or parsing fails.
func parseTFVarsMap(content, key string) map[string]string {
	result := make(map[string]string)

	re := regexp.MustCompile(`(?m)^` + key + `\s*=\s*\{`)
	loc := re.FindStringIndex(content)
	if loc == nil {
		return result
	}

	// Find the matching closing brace using brace counting
	start := loc[1] - 1 // position of the opening '{'
	depth := 0
	end := -1
	for i := start; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
				i = len(content) // break out of loop
			}
		}
	}
	if end == -1 {
		return result
	}

	body := content[start+1 : end]

	// Extract key = "value" pairs
	pairRe := regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"`)
	for _, m := range pairRe.FindAllStringSubmatch(body, -1) {
		result[m[1]] = m[2]
	}

	return result
}
