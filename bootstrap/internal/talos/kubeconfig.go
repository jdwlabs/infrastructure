package talos

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// KubeconfigManager handles fetching, merging, and managing kubeconfig files
type KubeconfigManager struct {
	client *Client
	logger *zap.Logger
}

// NewKubeconfigManager creates a new kubeconfig manager
func NewKubeconfigManager(client *Client, logger *zap.Logger) *KubeconfigManager {
	return &KubeconfigManager{
		client: client,
		logger: logger,
	}
}

// kubeConfig represents the structure of a kubeconfig file
type kubeConfig struct {
	APIVersion     string        `yaml:"apiVersion"`
	Kind           string        `yaml:"kind"`
	CurrentContext string        `yaml:"current-context"`
	Clusters       []kubeCluster `yaml:"clusters"`
	Contexts       []kubeContext `yaml:"contexts"`
	Users          []kubeUser    `yaml:"users"`
}

type kubeCluster struct {
	Name    string            `yaml:"name"`
	Cluster kubeClusterDetail `yaml:"cluster"`
}

type kubeClusterDetail struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data,omitempty"`
}

type kubeContext struct {
	Name    string            `yaml:"name"`
	Context kubeContextDetail `yaml:"context"`
}

type kubeContextDetail struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type kubeUser struct {
	Name string      `yaml:"name"`
	User interface{} `yaml:"user"`
}

// FetchAndMerge fetches kubeconfig from a healthy control plane, updates the server URL
// to use the FQDN endpoint, renames the context to the cluster name, and merges it with
// the user's existing kubeconfig.
//
// haproxyIP is the HAProxy load-balancer IP. We first verify the Kubernetes API is
// reachable via the IP (before DNS may be configured) by fetching via IP, then set
// the final server URL to the FQDN endpoint.
func (km *KubeconfigManager) FetchAndMerge(ctx context.Context, endpoint net.IP, clusterName string, controlPlaneEndpoint string) error {
	km.logger.Info("fetching kubeconfig",
		zap.String("endpoint", endpoint.String()),
		zap.String("cluster", clusterName))

	// Verify API reachability via the direct CP IP before relying on DNS
	if err := km.verifyKubernetesAPI(ctx, endpoint); err != nil {
		return fmt.Errorf("kubernetes API not reachable at %s: %w", endpoint, err)
	}
	km.logger.Debug("kubernetes API reachable via CP IP", zap.String("ip", endpoint.String()))

	// 1. Fetch raw kubeconfig to a temp file
	tmpDir, err := os.MkdirTemp("", "talos-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, "kubeconfig")
	if err := km.client.Kubeconfig(ctx, endpoint, tmpPath); err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}

	// 2. Read and parse the fetched kubeconfig
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("read fetched kubeconfig: %w", err)
	}

	var kc kubeConfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return fmt.Errorf("parse kubeconfig YAML: %w", err)
	}

	// 3. Update server URL to use FQDN
	newServer := fmt.Sprintf("https://%s:6443", controlPlaneEndpoint)
	for i := range kc.Clusters {
		kc.Clusters[i].Cluster.Server = newServer
		kc.Clusters[i].Name = clusterName
	}

	// 4. Rename context to cluster name
	for i := range kc.Contexts {
		kc.Contexts[i].Name = clusterName
		kc.Contexts[i].Context.Cluster = clusterName
		// Keep the user name from the original
	}
	kc.CurrentContext = clusterName

	// 5. Rename user to include cluster name for uniqueness
	for i := range kc.Users {
		oldName := kc.Users[i].Name
		kc.Users[i].Name = clusterName + "-admin"
		// Update context to reference new user name
		for j := range kc.Contexts {
			if kc.Contexts[j].Context.User == oldName {
				kc.Contexts[j].Context.User = kc.Users[i].Name
			}
		}
	}

	// 6. Write modified kubeconfig back to temp file
	modifiedData, err := yaml.Marshal(&kc)
	if err != nil {
		return fmt.Errorf("marshal modified kubeconfig: %w", err)
	}
	modifiedPath := filepath.Join(tmpDir, "kubeconfig-modified")
	if err := os.WriteFile(modifiedPath, modifiedData, 0600); err != nil {
		return fmt.Errorf("write modified kubeconfig: %w", err)
	}

	// 7. Merge with existing kubeconfig using kubectl
	existingPath := km.kubeconfigPath()
	if _, statErr := os.Stat(existingPath); statErr == nil {
		backupPath := existingPath + ".backup"
		if backupData, readErr := os.ReadFile(existingPath); readErr == nil {
			if writeErr := os.WriteFile(backupPath, backupData, 0600); writeErr != nil {
				km.logger.Warn("failed to backup kubeconfig", zap.Error(writeErr))
			} else {
				km.logger.Debug("backed up kubeconfig", zap.String("path", backupPath))
			}
		}
	}

	// 8. Merge with existing kubeconfig using kubectl
	if err := km.mergeKubeconfig(existingPath, modifiedPath); err != nil {
		// If merge fails, just write the new kubeconfig directly
		km.logger.Warn("merge failed, writing kubeconfig directly", zap.Error(err))
		if err := km.writeDirectly(existingPath, modifiedData); err != nil {
			return fmt.Errorf("write modified kubeconfig: %w", err)
		}
	}

	km.logger.Info("kubeconfig updated",
		zap.String("path", existingPath),
		zap.String("context", clusterName))

	return nil
}

// Verify checks that kubectl can communicate with the cluster
func (km *KubeconfigManager) Verify(ctx context.Context, clusterName string) error {
	km.logger.Info("verifying Kubernetes access", zap.String("context", clusterName))

	args := []string{"--context", clusterName, "cluster-info"}
	var output []byte
	var err error

	if km.client != nil && km.client.audit != nil {
		ac := km.client.audit.CommandContext(ctx, "kubectl", args...)
		ac.Env = append(os.Environ(), "KUBECONFIG="+km.kubeconfigPath())
		output, err = ac.CombinedOutput()
	} else {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+km.kubeconfigPath())
		output, err = cmd.CombinedOutput()
	}

	if err != nil {
		return fmt.Errorf("kubectl %s: %w, output: %s", strings.Join(args, " "), err, string(output))
	}

	km.logger.Info("Kubernetes cluster is accessible", zap.String("output", string(output)))
	return nil
}

// kubeconfigPath returns the path to the kubeconfig file
func (km *KubeconfigManager) kubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		// Use filepath.SplitList to handle OS-specific separators
		// (semicolon on Windows, colon on Unix)
		parts := filepath.SplitList(v)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback: try environment variables
		if h := os.Getenv("HOME"); h != "" {
			return filepath.Join(h, ".kube", "config")
		}
		if h := os.Getenv("USERPROFILE"); h != "" {
			return filepath.Join(h, ".kube", "config")
		}
		return filepath.Join(string(filepath.Separator), "root", ".kube", "config")
	}
	return filepath.Join(home, ".kube", "config")
}

func (km *KubeconfigManager) mergeKubeconfig(existingPath, newPath string) error {
	// Ensure the target directory exists
	dir := filepath.Dir(existingPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}

	// If existing kubeconfig doesn't exist, just copy the new one into place
	if _, err := os.Stat(existingPath); os.IsNotExist(err) {
		data, err := os.ReadFile(newPath)
		if err != nil {
			return err
		}
		return os.WriteFile(existingPath, data, 0600)
	}

	// Read the new kubeconfig to find which cluster/context/user names it defines
	newData, err := os.ReadFile(newPath)
	if err != nil {
		return fmt.Errorf("read new kubeconfig: %w", err)
	}
	var newKC kubeConfig
	if err := yaml.Unmarshal(newData, &newKC); err != nil {
		return fmt.Errorf("parse new kubeconfig: %w", err)
	}

	// Build sets of names to strip from existing config
	clusterNames := make(map[string]bool)
	contextNames := make(map[string]bool)
	userNames := make(map[string]bool)
	for _, c := range newKC.Clusters {
		clusterNames[c.Name] = true
	}
	for _, c := range newKC.Contexts {
		contextNames[c.Name] = true
	}
	for _, u := range newKC.Users {
		userNames[u.Name] = true
	}

	// Read and strip conflicting entries from existing kubeconfig so the fresh
	// Talos entries take precedence (kubectl config view uses first-wins semantics)
	existingData, err := os.ReadFile(existingPath)
	if err != nil {
		return fmt.Errorf("read existing kubeconfig: %w", err)
	}
	var existingKC kubeConfig
	if err := yaml.Unmarshal(existingData, &existingKC); err != nil {
		return fmt.Errorf("parse existing kubeconfig: %w", err)
	}

	filtered := false
	var keptClusters []kubeCluster
	for _, c := range existingKC.Clusters {
		if clusterNames[c.Name] {
			filtered = true
			continue
		}
		keptClusters = append(keptClusters, c)
	}
	existingKC.Clusters = keptClusters

	var keptContexts []kubeContext
	for _, c := range existingKC.Contexts {
		if contextNames[c.Name] {
			filtered = true
			continue
		}
		keptContexts = append(keptContexts, c)
	}
	existingKC.Contexts = keptContexts

	var keptUsers []kubeUser
	for _, u := range existingKC.Users {
		if userNames[u.Name] {
			filtered = true
			continue
		}
		keptUsers = append(keptUsers, u)
	}
	existingKC.Users = keptUsers

	if filtered {
		km.logger.Debug("stripped stale entries from existing kubeconfig before merge",
			zap.Any("clusters", clusterNames),
			zap.Any("contexts", contextNames),
			zap.Any("users", userNames))
	}

	// Write the cleaned existing config to a temp file fro merging
	cleanedExistingPath := existingPath + ".cleaned"
	cleanedData, err := yaml.Marshal(&existingKC)
	if err != nil {
		return fmt.Errorf("marshal cleaned kubeconfig: %w", err)
	}
	if err := os.WriteFile(cleanedExistingPath, cleanedData, 0600); err != nil {
		return fmt.Errorf("write cleaned kubeconfig: %w", err)
	}
	defer os.Remove(cleanedExistingPath)

	// Use KUBECONFIG env var to merge using kubectl (new config first for precedence)
	mergedPath := existingPath + ".merged"
	args := []string{"config", "view", "--flatten"}
	kubeconfigEnv := fmt.Sprintf("KUBECONFIG=%s%s%s", newPath, string(filepath.ListSeparator), cleanedExistingPath)

	var output []byte
	if km.client != nil && km.client.audit != nil {
		ac := km.client.audit.Command("kubectl", args...)
		ac.Env = append(os.Environ(), kubeconfigEnv)
		output, err = ac.CombinedOutput()
	} else {
		cmd := exec.Command("kubectl", args...)
		cmd.Env = append(os.Environ(), kubeconfigEnv)
		output, err = cmd.CombinedOutput()
	}
	if err != nil {
		return fmt.Errorf("kubectl %s: %w, output: %s", strings.Join(args, " "), err, string(output))
	}

	if err := os.WriteFile(mergedPath, output, 0600); err != nil {
		return err
	}

	// Atomic replace
	return os.Rename(mergedPath, existingPath)
}

func (km *KubeconfigManager) writeDirectly(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// WaitForKubernetesAPI polls port 6443 on the given IP until it accepts a TCP
// connection or the timeout expires. After an etcd bootstrap the K8s API server
// can take 1-3 minutes to start; this replaces the single-shot probe in
// FetchAndMerge with a patient retry loop.
func (km *KubeconfigManager) WaitForKubernetesAPI(ctx context.Context, ip net.IP, maxWait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	addr := fmt.Sprintf("%s:6443", ip)
	km.logger.Info("waiting for Kubernetes API to become reachable",
		zap.String("addr", addr),
		zap.Duration("timeout", maxWait))

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Try immediately, then on each tick
	for attempt := 1; ; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			conn.Close()
			km.logger.Info("Kubernetes API reachable", zap.String("addr", addr), zap.Int("attempt", attempt))
			return nil
		}
		km.logger.Debug("Kubernetes API not yet reachable", zap.String("addr", addr), zap.Int("attempt", attempt), zap.Error(err))

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for Kubernetes API at %s after %s: %w", addr, maxWait, err)
		case <-ticker.C:
		}
	}
}

// verifyKubernetesAPI checks that the Kubernetes API port (6443) is reachable on the
// given IP. This is a lightweight TCP probe - it does not authenticate.
func (km *KubeconfigManager) verifyKubernetesAPI(ctx context.Context, ip net.IP) error {
	addr := fmt.Sprintf("%s:6443", ip)
	deadline, ok := ctx.Deadline()
	timeout := 10 * time.Second
	if ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("tcp dial %s: %w", addr, err)
	}
	conn.Close()
	return nil
}
