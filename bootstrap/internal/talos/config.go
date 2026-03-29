package talos

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/logging"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

//go:embed patches/control-plane.yaml
var controlPlanePatchTemplate string

//go:embed patches/worker.yaml
var workerPatchTemplate string

// NodeConfig handles generation and management of Talos node configurations
type NodeConfig struct {
	cfg   *types.Config
	audit *logging.AuditLogger
}

// NewNodeConfig creates a new NodeConfig generator
func NewNodeConfig(cfg *types.Config) *NodeConfig {
	return &NodeConfig{cfg: cfg}
}

// SetAuditLogger attaches an audit logger for command tracking
func (nc *NodeConfig) SetAuditLogger(audit *logging.AuditLogger) {
	nc.audit = audit
}

// execCommandAudited runs a command with full audit logging if available, returning combined output
func (nc *NodeConfig) execCommandAudited(name string, args ...string) ([]byte, error) {
	if nc.audit != nil {
		ac := nc.audit.Command(name, args...)
		return ac.CombinedOutput()
	}
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}

// templateData holds the values for config template rendering
type templateData struct {
	Hostname                string
	DefaultDisk             string
	DefaultNetworkInterface string
	HAProxyIP               string
	ControlPlaneEndpoint    string
	InstallerImage          string
	ClusterName             string
}

// GenerateBaseConfigs generate cluster secrets and base configs using talosctl.
// This produces secrets.yaml, control-plane.yaml, worker.yaml, and talosconfig in SecretsDir
func (nc *NodeConfig) GenerateBaseConfigs() error {
	secretsDir := nc.cfg.SecretsDir
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		return fmt.Errorf("cannot create secrets directory %s: %w", secretsDir, err)
	}

	secretsFile := filepath.Join(secretsDir, "secrets.yaml")

	// Generate secrets if they don't exist
	if _, err := os.Stat(secretsFile); os.IsNotExist(err) {
		output, err := nc.execCommandAudited("talosctl", "gen", "secrets", "-o", secretsFile)
		if err != nil {
			return fmt.Errorf("talosctl gen secrets: %w, output: %s", err, string(output))
		}
		if err := os.Chmod(secretsFile, 0600); err != nil {
			return fmt.Errorf("cannot chmod secrets: %w", err)
		}
	}

	// Generate base configs using the secrets
	additionalSANs := fmt.Sprintf("%s,%s,127.0.0.1",
		nc.cfg.HAProxyIP.String(), nc.cfg.ControlPlaneEndpoint)
	clusterEndpoint := fmt.Sprintf("https://%s:6443", nc.cfg.ControlPlaneEndpoint)

	// Run talosctl gen config in a temp dir, then move the output files
	tmpDir, err := os.MkdirTemp("", "talos-gen-config-*")
	if err != nil {
		return fmt.Errorf("cannot create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	output, err := nc.execCommandAudited("talosctl", "gen", "config",
		"--with-secrets", secretsFile,
		"--kubernetes-version", nc.cfg.KubernetesVersion,
		"--talos-version", nc.cfg.TalosVersion,
		"--install-image", nc.cfg.InstallerImage,
		"--additional-sans", additionalSANs,
		"--output-dir", tmpDir,
		nc.cfg.ClusterName, clusterEndpoint,
	)
	if err != nil {
		return fmt.Errorf("talosctl gen config: %w, output: %s", err, string(output))
	}

	// Move generated files to secrets dir
	moves := map[string]string{
		"controlplane.yaml": filepath.Join(secretsDir, "control-plane.yaml"),
		"worker.yaml":       filepath.Join(secretsDir, "worker.yaml"),
		"talosconfig":       filepath.Join(secretsDir, "talosconfig"),
	}

	for src, dst := range moves {
		srcPath := filepath.Join(tmpDir, src)
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", srcPath, err)
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return fmt.Errorf("cannot write %s: %w", dst, err)
		}
	}

	return nil
}

// resolvePatchTemplate resolves the role patch template using the override chain:
// 1. --patch-dir flag (highest precedence)
// 2. clusters/<cluster-name>/patches/ (per-cluster overrides)
// 3. Embedded defaults (always present)
// Returns the template content and the source description.
func resolvePatchTemplate(role types.Role, patchDir, clusterDir string) (string, string) {
	var filename string
	switch role {
	case types.RoleControlPlane:
		filename = "control-plane.yaml"
	case types.RoleWorker:
		filename = "worker.yaml"
	}

	// 1. Explicit --patch-dir
	if patchDir != "" {
		path := filepath.Join(patchDir, filename)
		if data, err := os.ReadFile(path); err == nil {
			return string(data), path
		}
	}

	// 2. Per-cluster override
	if clusterDir != "" {
		path := filepath.Join(clusterDir, "patches", filename)
		if data, err := os.ReadFile(path); err == nil {
			return string(data), path
		}
	}

	// 3. Embedded default
	switch role {
	case types.RoleControlPlane:
		return controlPlanePatchTemplate, "embedded:control-plane.yaml"
	default:
		return workerPatchTemplate, "embedded:worker.yaml"
	}
}

// resolveNodePatch finds a per-node patch file (plain YAML, not Go template).
// Searches --patch-dir then clusters/<cluster>/patches/ for node-<vmid>.yaml.
// Returns empty string if no per-node patch exists.
func resolveNodePatch(vmid types.VMID, patchDir, clusterDir string) string {
	filename := fmt.Sprintf("node-%d.yaml", vmid)

	// 1. Explicit --patch-dir
	if patchDir != "" {
		path := filepath.Join(patchDir, filename)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// 2. Per-cluster override
	if clusterDir != "" {
		path := filepath.Join(clusterDir, "patches", filename)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// Generate creates a Talos node config by generating a patch file and applying it
// to the base config using talosctl machineconfig patch. Returns the SHA256 hash.
// Uses the patch override chain: --patch-dir > clusters/<cluster>/patches/ > embedded.
func (nc *NodeConfig) Generate(spec *types.NodeSpec, outputDir string) (string, error) {
	data := templateData{
		Hostname:                spec.Name,
		DefaultDisk:             nc.cfg.DefaultDisk,
		DefaultNetworkInterface: nc.cfg.DefaultNetworkInterface,
		HAProxyIP:               nc.cfg.HAProxyIP.String(),
		ControlPlaneEndpoint:    nc.cfg.ControlPlaneEndpoint,
		InstallerImage:          nc.cfg.InstallerImage,
		ClusterName:             nc.cfg.ClusterName,
	}

	var baseConfigName string
	switch spec.Role {
	case types.RoleControlPlane:
		baseConfigName = "control-plane.yaml"
	case types.RoleWorker:
		baseConfigName = "worker.yaml"
	default:
		return "", fmt.Errorf("unknown node role: %s", spec.Role)
	}

	// Ensure base configs exists
	baseConfig := filepath.Join(nc.cfg.SecretsDir, baseConfigName)
	if _, err := os.Stat(baseConfig); os.IsNotExist(err) {
		if err := nc.GenerateBaseConfigs(); err != nil {
			return "", fmt.Errorf("cannot generate base configs: %w", err)
		}
	}

	// Resolve role patch template from override chain
	clusterDir := filepath.Join("clusters", nc.cfg.ClusterName)
	tmplStr, _ := resolvePatchTemplate(spec.Role, nc.cfg.PatchDir, clusterDir)

	// Render patch template
	tmpl, err := template.New("nodePatch").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	// Write patch to temp file
	patchDir := filepath.Join(outputDir, ".patches")
	if err := os.MkdirAll(patchDir, 0755); err != nil {
		return "", fmt.Errorf("create patch dir: %w", err)
	}

	patchFile := filepath.Join(patchDir, fmt.Sprintf("%s-%d.yaml", spec.Role, spec.VMID))
	if err := os.WriteFile(patchFile, buf.Bytes(), 0600); err != nil {
		return "", fmt.Errorf("write patch file: %w", err)
	}
	defer os.Remove(patchFile)

	// Ensure output directory exists (restricted permissions - contains TLS material)
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	// Build patch args - role template first, then optional per-node patch
	patchArgs := []string{"--patch", "@" + patchFile}

	nodePatchPath := resolveNodePatch(spec.VMID, nc.cfg.PatchDir, clusterDir)
	if nodePatchPath != "" {
		patchArgs = append(patchArgs, "--patch", "@"+nodePatchPath)
	}

	// Apply patches using talosctl machineconfig patch
	outputPath := filepath.Join(outputDir, fmt.Sprintf("node-%s-%d.yaml", spec.Role, spec.VMID))
	args := []string{"machineconfig", "patch", baseConfig}
	args = append(args, patchArgs...)
	args = append(args, "--output", outputPath)

	output, err := nc.execCommandAudited("talosctl", args...)
	if err != nil {
		return "", fmt.Errorf("talosctl machineconfig patch: %w, output: %s", err, string(output))
	}

	// Compute SHA256 hash of the generated config
	configBytes, err := os.ReadFile(outputPath)
	if err != nil {
		return "", fmt.Errorf("read generated configs: %w", err)
	}
	hash := sha256.Sum256(configBytes)
	return hex.EncodeToString(hash[:]), nil
}

// HashFile computes the SHA256 hash of an existing config file for drift detection
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
