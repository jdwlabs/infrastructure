package types

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VMID is a typed integer to prevent mixing up VM IDs with other ints
type VMID int

func (v VMID) String() string {
	return fmt.Sprintf("%d", v)
}

// Role distinguishes control plane from worker
type Role string

const (
	RoleControlPlane Role = "control-plane"
	RoleWorker       Role = "worker"
)

// NodeSpec represents what Terraform wants (your DESIRED_*_VMIDS)
type NodeSpec struct {
	VMID   VMID   `json:"vmid" hcl:"vmid"`
	Name   string `json:"name" hcl:"vm_name"`
	Node   string `json:"node" hcl:"node_name"` // Proxmox node name (pve1, pve2, etc.)
	CPU    int    `json:"cpu" hcl:"cpu_cores"`
	Memory int    `json:"memory" hcl:"memory"`  // MB
	Disk   int    `json:"disk" hcl:"disk_size"` // GB
	Role   Role   `json:"role"`
}

// NodeState represents what we know is deployed (your DEPLOYED_*_IPS)
type NodeState struct {
	VMID       VMID       `json:"vmid"`
	IP         net.IP     `json:"ip,omitempty"`
	ConfigHash string     `json:"config_hash,omitempty"`
	MAC        string     `json:"mac,omitempty"` // For IP rediscovery
	LastSeen   time.Time  `json:"last_seen"`
	Role       Role       `json:"role,omitempty"`       // Stored for audit trail (self-describing history entries)
	RemovedAt  *time.Time `json:"removed_at,omitempty"` // Set when node is removed from cluster
}

// MarshalJSON customizes JSON serialization for NodeState
func (n NodeState) MarshalJSON() ([]byte, error) {
	type Alias NodeState
	var ipStr string
	if n.IP != nil {
		ipStr = n.IP.String()
	}
	return json.Marshal(&struct {
		IP string `json:"ip,omitempty"`
		*Alias
	}{
		IP:    ipStr,
		Alias: (*Alias)(&n),
	})
}

// UnmarshalJSON customizes JSON deserialization for NodeState
func (n *NodeState) UnmarshalJSON(data []byte) error {
	type Alias NodeState
	aux := &struct {
		IP string `json:"ip,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(n),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.IP != "" {
		n.IP = net.ParseIP(aux.IP)
	}
	return nil
}

// LiveNode represents current reality from Proxmox/Talos (your LIVE_NODE_*)
type LiveNode struct {
	VMID         VMID       `json:"vmid"`
	IP           net.IP     `json:"ip"`
	MAC          string     `json:"mac"`
	Status       NodeStatus `json:"status"`
	TalosVersion string     `json:"talos_version,omitempty"`
	K8sVersion   string     `json:"k8s_version,omitempty"`
	DiscoveredAt time.Time  `json:"discovered_at"`
}

type NodeStatus string

const (
	StatusDiscovered NodeStatus = "discovered"
	StatusJoined     NodeStatus = "joined" // In Talos cluster
	StatusReady      NodeStatus = "ready"  // Kubernetes ready
	StatusNotFound   NodeStatus = "not_found"
	StatusRebooting  NodeStatus = "rebooting" // Transient state
)

// ClusterState is your bootstrap-state.json as a typed struct
type ClusterState struct {
	Timestamp            time.Time   `json:"timestamp"`
	TerraformHash        string      `json:"terraform_hash"`
	ClusterName          string      `json:"cluster_name"`
	BootstrapCompleted   bool        `json:"bootstrap_completed"`
	FirstControlPlane    VMID        `json:"first_control_plane_vmid,omitempty"`
	ControlPlanes        []NodeState `json:"control_planes"`
	Workers              []NodeState `json:"workers"`
	RemovedNodes         []NodeState `json:"removed_nodes,omitempty"` // Audit trail of previously removed nodes
	HAProxyIP            net.IP      `json:"haproxy_ip"`
	ControlPlaneEndpoint string      `json:"control_plane_endpoint"`
	KubernetesVersion    string      `json:"kubernetes_version"`
	TalosVersion         string      `json:"talos_version"`
}

// ReconcilePlan replaces your PLAN_* arrays
type ReconcilePlan struct {
	NeedsBootstrap      bool   `json:"needs_bootstrap"`
	AddControlPlanes    []VMID `json:"add_control_planes"`
	AddWorkers          []VMID `json:"add_workers"`
	RemoveControlPlanes []VMID `json:"remove_control_planes"`
	RemoveWorkers       []VMID `json:"remove_workers"`
	UpdateConfigs       []VMID `json:"update_configs"`
	NoOp                []VMID `json:"noop"`
}

// IsEmpty returns true if no operations are planned
func (p *ReconcilePlan) IsEmpty() bool {
	return !p.NeedsBootstrap &&
		len(p.AddControlPlanes) == 0 &&
		len(p.AddWorkers) == 0 &&
		len(p.RemoveControlPlanes) == 0 &&
		len(p.RemoveWorkers) == 0 &&
		len(p.UpdateConfigs) == 0
}

// Config represents your terraform.tfvars + environment variables
type Config struct {
	ClusterName             string `json:"cluster_name"`
	TerraformTFVars         string `json:"terraform_tfvars"`
	ControlPlaneEndpoint    string `json:"control_plane_endpoint"`
	HAProxyIP               net.IP `json:"haproxy_ip"`
	HAProxyLoginUser        string `json:"haproxy_login_username"`
	HAProxySSHKeyPath       string `json:"haproxy_ssh_key_path"`
	HAProxyStatsUser        string `json:"haproxy_stats_username"`
	HAProxyStatsPassword    string `json:"haproxy_stats_password"`
	KubernetesVersion       string `json:"kubernetes_version"`
	TalosVersion            string `json:"talos_version"`
	InstallerImage          string `json:"installer_image"`
	DefaultNetworkInterface string `json:"default_network_interface"`
	DefaultDisk             string `json:"default_disk"`
	SecretsDir              string `json:"secrets_dir"`

	// Ingress NodePorts for the cluster's edge gateway (e.g. NGINX Gateway Fabric).
	// HAProxy forwards :80/:443 to these NodePorts on every node.
	IngressHTTPNodePort int `json:"ingress_http_node_port"`
	IngressTLSNodePort  int `json:"ingress_tls_node_port"`

	// Proxmox connection
	ProxmoxSSHUser     string            `json:"proxmox_ssh_user"`
	ProxmoxSSHHost     string            `json:"proxmox_ssh_host"`
	ProxmoxSSHKeyPath  string            `json:"proxmox_ssh_key_path"`
	ProxmoxNodeIPs     map[string]net.IP `json:"proxmox_node_ips"` // pve1 -> 192.168.1.200
	ProxmoxTokenID     string            `json:"proxmox_token_id,omitempty"`
	ProxmoxTokenSecret string            `json:"proxmox_token_secret,omitempty"`

	// Runtime flags
	InsecureSSH      bool   `json:"insecure_ssh"` // Skip SSH host key verification
	AutoApprove      bool   `json:"auto_approve"`
	DryRun           bool   `json:"dry_run"`
	PlanMode         bool   `json:"plan_mode"`
	SkipPreflight    bool   `json:"skip_preflight"`
	ForceReconfigure bool   `json:"force_reconfigure"`
	LogLevel         string `json:"log_level"`
	LogDir           string `json:"log_dir"`
	NoColor          bool   `json:"no_color"`

	// Infra management
	TerraformDir string `json:"terraform_dir,omitempty"` // Directory containing .tf files
	SkipBackup   bool   `json:"skip_backup,omitempty"`

	// Patch overrides
	PatchDir string `json:"patch_dir,omitempty"` // Directory for patch template overrides

	// Internal
	TerraformHash string `json:"-"` // Computed, not serialized
}

// InfraDeployState tracks infrastructure deployment metadata
type InfraDeployState struct {
	Timestamp        string `json:"timestamp"`
	TerraformVersion string `json:"terraform_version"`
	AutoApproved     bool   `json:"auto_approved"`
	LastDeployment   string `json:"last_deployment"`
}

// DefaultConfig returns a config with sensible defaults.
// Infrastructure-specific values (IPs, endpoints, credentials) are left
// empty and must be provided via flags, environment variables, or tfvars.
func DefaultConfig() *Config {
	cfg := &Config{
		ClusterName:             "cluster",
		TerraformTFVars:         "terraform.tfvars",
		DefaultNetworkInterface: "eth0",
		DefaultDisk:             "sda",
		ProxmoxSSHUser:          "root",
		ProxmoxSSHKeyPath:       defaultSSHKeyPath(),
		ProxmoxNodeIPs:          map[string]net.IP{},
		LogLevel:                "info",
		LogDir:                  "logs",
		NoColor:                 isNoColorEnv(),
	}
	cfg.SecretsDir = filepath.Join("clusters", cfg.ClusterName, "secrets")
	return cfg
}

// Validate checks that required configuration fields are set.
// Called after all config sources (flag, env vars, tfvars) have been merged.
func (c *Config) Validate() error {
	var missing []string
	if c.ControlPlaneEndpoint == "" {
		missing = append(missing, "control-plane-endpoint (CONTROL_PLANE_ENDPOINT)")
	}
	if c.HAProxyIP == nil {
		missing = append(missing, "haproxy-ip (HA_PROXY_IP)")
	}
	if c.KubernetesVersion == "" {
		missing = append(missing, "kubernetes-version (KUBERNETES_VERSION)")
	}
	if c.TalosVersion == "" {
		missing = append(missing, "talos-version (TALOS_VERSION)")
	}
	if c.InstallerImage == "" {
		missing = append(missing, "installer-image (INSTALLER_IMAGE)")
	}
	if len(c.ProxmoxNodeIPs) == 0 {
		missing = append(missing, "proxmox-node-ips (configure via tfvars or flags)")
	}
	if c.IngressHTTPNodePort <= 0 {
		missing = append(missing, "ingress-http-nodeport (INGRESS_HTTP_NODEPORT)")
	}
	if c.IngressTLSNodePort <= 0 {
		missing = append(missing, "ingress-tls-nodeport (INGRESS_TLS_NODEPORT)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("required configuration missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// TestConfig returns a Config pre-populated with test/example values.
// Use in tests instead of DefaultConfig to avoid validation faliures.
func TestConfig() *Config {
	cfg := DefaultConfig()
	cfg.ControlPlaneEndpoint = "cluster.example.com"
	cfg.HAProxyIP = net.ParseIP("192.168.1.199")
	cfg.HAProxyLoginUser = "root"
	cfg.HAProxyStatsUser = "admin"
	cfg.HAProxyStatsPassword = "admin"
	cfg.KubernetesVersion = "v1.35.1"
	cfg.TalosVersion = "v1.12.3"
	cfg.InstallerImage = "factory.talos.dev/nocloud-installer/test:v1.12.3"
	cfg.ProxmoxNodeIPs = map[string]net.IP{
		"pve1": net.ParseIP("192.168.1.200"),
		"pve2": net.ParseIP("192.168.1.201"),
	}
	cfg.IngressHTTPNodePort = 30180
	cfg.IngressTLSNodePort = 30543
	return cfg
}

// isNoColorEnv checks environment variables for color suppression.
// Respects the NO_COLOR standard (https://no-color.org/) and TERM=dumb.
func isNoColorEnv() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	return false
}

func defaultSSHKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback: use home dir from environment
		if h := os.Getenv("HOME"); h != "" {
			return filepath.Join(h, ".ssh", "id_rsa")
		}
		if h := os.Getenv("USERPROFILE"); h != "" {
			return filepath.Join(h, ".ssh", "id_rsa")
		}
		return filepath.Join(string(filepath.Separator), "root", ".ssh", "id_rsa")
	}
	return filepath.Join(home, ".ssh", "id_rsa")
}
