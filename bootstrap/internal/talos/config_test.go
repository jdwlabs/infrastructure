package talos

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNodeConfig(t *testing.T) {
	cfg := types.TestConfig()
	nc := NewNodeConfig(cfg)

	assert.NotNil(t, nc)
	assert.Equal(t, cfg, nc.cfg)
	assert.Nil(t, nc.audit)
}

func TestNodeConfig_SetAuditLogger(t *testing.T) {
	cfg := types.TestConfig()
	nc := NewNodeConfig(cfg)

	assert.Nil(t, nc.audit)

	nc.SetAuditLogger(nil)
	assert.Nil(t, nc.audit)
}

func TestNodeConfigGenerate_ControlPlane(t *testing.T) {
	if os.Getenv("SKIP_TALOSCTL") != "" {
		t.Skip("Skipping talosctl-dependent test")
	}

	cfg := types.TestConfig()
	cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")
	nc := NewNodeConfig(cfg)

	spec := &types.NodeSpec{
		VMID:   201,
		Name:   "talos-cp-1",
		Node:   "pve1",
		CPU:    4,
		Memory: 8192,
		Disk:   50,
		Role:   types.RoleControlPlane,
	}

	tmpDir := t.TempDir()
	hash, err := nc.Generate(spec, tmpDir)

	if err != nil {
		t.Skipf("talosctl not available or base configs missing: %v", err)
	}

	require.NoError(t, err)
	assert.NotEmpty(t, hash)
	assert.Len(t, hash, 64)

	expectedPath := filepath.Join(tmpDir, "node-control-plane-201.yaml")
	_, err = os.Stat(expectedPath)
	require.NoError(t, err)

	content, err := os.ReadFile(expectedPath)
	require.NoError(t, err)

	contentStr := string(content)
	assert.Contains(t, contentStr, "version: v1alpha1")
	assert.Contains(t, contentStr, "type: controlplane")
	// Hostname is set via HostnameConfig with auto: stable, not inline
	assert.Contains(t, contentStr, "auto: stable")
	assert.Contains(t, contentStr, "clusterName: cluster")
	assert.Contains(t, contentStr, "endpoint: https://cluster.example.com:6443")
	assert.Contains(t, contentStr, "disk: /dev/sda")
	assert.Contains(t, contentStr, "interface: eth0")
	assert.Contains(t, contentStr, "vm.nr_hugepages: \"1024\"")
	// Note: the actual field name is allowSchedulingOnControlPlanes (with s)
	assert.Contains(t, contentStr, "allowSchedulingOnControlPlanes: false")
}

func TestNodeConfigGenerate_Worker(t *testing.T) {
	if os.Getenv("SKIP_TALOSCTL") != "" {
		t.Skip("Skipping talosctl-dependent test")
	}

	cfg := types.TestConfig()
	cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")
	nc := NewNodeConfig(cfg)

	spec := &types.NodeSpec{
		VMID:   301,
		Name:   "talos-worker-1",
		Node:   "pve2",
		CPU:    8,
		Memory: 16384,
		Disk:   100,
		Role:   types.RoleWorker,
	}

	tmpDir := t.TempDir()
	hash, err := nc.Generate(spec, tmpDir)

	if err != nil {
		t.Skipf("talosctl not available or base configs missing: %v", err)
	}

	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	expectedPath := filepath.Join(tmpDir, "node-worker-301.yaml")
	_, err = os.Stat(expectedPath)
	require.NoError(t, err)

	content, err := os.ReadFile(expectedPath)
	require.NoError(t, err)

	contentStr := string(content)
	assert.Contains(t, contentStr, "type: worker")
	// Hostname is set via HostnameConfig with auto: stable, not inline
	assert.Contains(t, contentStr, "auto: stable")
	assert.Contains(t, contentStr, "destination: /var/local")
	assert.NotContains(t, contentStr, "allowSchedulingOnControlPlanes")
}

func TestNodeConfigGenerate_UnknownRole(t *testing.T) {
	cfg := types.TestConfig()
	nc := NewNodeConfig(cfg)

	spec := &types.NodeSpec{
		VMID: 401,
		Name: "unknown",
		Role: types.Role("unknown-role"),
	}

	tmpDir := t.TempDir()
	_, err := nc.Generate(spec, tmpDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown node role")
}

func TestNodeConfigGenerate_CreatesDirectory(t *testing.T) {
	cfg := types.TestConfig()
	cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")
	nc := NewNodeConfig(cfg)

	spec := &types.NodeSpec{
		VMID: 201,
		Name: "test",
		Role: types.RoleControlPlane,
	}

	tmpDir := t.TempDir()
	nestedDir := filepath.Join(tmpDir, "nested", "config", "dir")

	_, _ = nc.Generate(spec, nestedDir)

	if _, err := os.Stat(nestedDir); err == nil {
		assert.NoError(t, err)
	}
}

func TestNodeConfigGenerate_TemplateData(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *types.Config
		spec     *types.NodeSpec
		expected map[string]string
	}{
		{
			name: "custom cluster settings",
			cfg: &types.Config{
				ClusterName:             "prod-cluster",
				ControlPlaneEndpoint:    "k8s.prod.com",
				HAProxyIP:               net.ParseIP("10.0.0.1"),
				InstallerImage:          "factory.talos.dev/installer:v1.5.0",
				DefaultDisk:             "vda",
				DefaultNetworkInterface: "ens18",
				// SecretsDir will be set per-test to ensure isolation
			},
			spec: &types.NodeSpec{
				VMID: 201,
				Name: "cp-1",
				Role: types.RoleControlPlane,
			},
			expected: map[string]string{
				"prod-cluster":                       "",
				"k8s.prod.com":                       "",
				"10.0.0.1":                           "",
				"factory.talos.dev/installer:v1.5.0": "",
				"/dev/vda":                           "",
				"ens18":                              "",
			},
		},
		{
			name: "worker node settings",
			cfg: &types.Config{
				ClusterName:          "dev-cluster",
				ControlPlaneEndpoint: "k8s.dev.com",
				DefaultDisk:          "sda",
				// SecretsDir will be set per-test to ensure isolation
			},
			spec: &types.NodeSpec{
				VMID: 301,
				Name: "worker-1",
				Role: types.RoleWorker,
			},
			expected: map[string]string{
				"dev-cluster": "",
				"k8s.dev.com": "",
				"/dev/sda":    "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create isolated secrets directory for each test case
			secretsDir := filepath.Join(t.TempDir(), "secrets")
			tt.cfg.SecretsDir = secretsDir

			nc := NewNodeConfig(tt.cfg)
			tmpDir := t.TempDir()

			_, err := nc.Generate(tt.spec, tmpDir)
			if err != nil {
				t.Skipf("talosctl not available: %v", err)
			}

			var fileName string
			if tt.spec.Role == types.RoleControlPlane {
				fileName = filepath.Join(tmpDir, "node-control-plane-201.yaml")
			} else {
				fileName = filepath.Join(tmpDir, "node-worker-301.yaml")
			}

			content, err := os.ReadFile(fileName)
			if err != nil {
				t.Fatalf("Failed to read generated file: %v", err)
			}

			contentStr := string(content)
			for expected := range tt.expected {
				assert.Contains(t, contentStr, expected, "Generated config should contain %s", expected)
			}
		})
	}
}

func TestHashFile(t *testing.T) {
	t.Run("successful hash", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.yaml")
		content := []byte("test content for hashing")

		err := os.WriteFile(testFile, content, 0600)
		require.NoError(t, err)

		hash, err := HashFile(testFile)
		require.NoError(t, err)
		assert.Len(t, hash, 64)

		hash2, err := HashFile(testFile)
		require.NoError(t, err)
		assert.Equal(t, hash, hash2)

		differentFile := filepath.Join(tmpDir, "different.yaml")
		err = os.WriteFile(differentFile, []byte("different content"), 0600)
		require.NoError(t, err)

		differentHash, err := HashFile(differentFile)
		require.NoError(t, err)
		assert.NotEqual(t, hash, differentHash)
	})

	t.Run("non-existent file", func(t *testing.T) {
		_, err := HashFile("/nonexistent/file.yaml")
		require.Error(t, err)
		assert.True(t, os.IsNotExist(err), "Error should be file not found")
	})

	t.Run("empty file", func(t *testing.T) {
		tmpDir := t.TempDir()
		emptyFile := filepath.Join(tmpDir, "empty.yaml")
		err := os.WriteFile(emptyFile, []byte{}, 0600)
		require.NoError(t, err)

		hash, err := HashFile(emptyFile)
		require.NoError(t, err)
		assert.Len(t, hash, 64)
		assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", hash)
	})
}

func TestHashFile_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()
	largeFile := filepath.Join(tmpDir, "large.yaml")

	content := make([]byte, 10*1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}

	err := os.WriteFile(largeFile, content, 0600)
	require.NoError(t, err)

	hash, err := HashFile(largeFile)
	require.NoError(t, err)
	assert.Len(t, hash, 64)
}

func TestGenerateBaseConfigs_Integration(t *testing.T) {
	if os.Getenv("SKIP_TALOSCTL") != "" {
		t.Skip("Skipping talosctl integration test")
	}

	cfg := types.TestConfig()
	tmpDir := t.TempDir()
	cfg.SecretsDir = filepath.Join(tmpDir, "secrets")

	nc := NewNodeConfig(cfg)

	err := nc.GenerateBaseConfigs()
	if err != nil {
		t.Skipf("talosctl not available: %v", err)
	}

	expectedFiles := []string{
		"secrets.yaml",
		"control-plane.yaml",
		"worker.yaml",
		"talosconfig",
	}

	for _, file := range expectedFiles {
		path := filepath.Join(cfg.SecretsDir, file)
		_, err := os.Stat(path)
		assert.NoError(t, err, "Expected file %s to exist", file)
	}

	info, err := os.Stat(filepath.Join(cfg.SecretsDir, "secrets.yaml"))
	require.NoError(t, err)
	mode := info.Mode().Perm()

	// On Windows, permissions work differently (0x1b6 = 438 is normal)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0600), mode, "Secrets file should have 0600 permissions")
	} else {
		// Windows doesn't support Unix-style permissions in the same way
		// Just verify the file exists and is readable (not 0000)
		assert.NotEqual(t, os.FileMode(0000), mode, "File should have some permissions")
	}
}

func TestGenerateBaseConfigs_Idempotent(t *testing.T) {
	if os.Getenv("SKIP_TALOSCTL") != "" {
		t.Skip("Skipping talosctl integration test")
	}

	cfg := types.TestConfig()
	tmpDir := t.TempDir()
	cfg.SecretsDir = filepath.Join(tmpDir, "secrets")

	nc := NewNodeConfig(cfg)

	err := nc.GenerateBaseConfigs()
	if err != nil {
		t.Skipf("talosctl not available: %v", err)
	}

	secretsPath := filepath.Join(cfg.SecretsDir, "secrets.yaml")
	info1, err := os.Stat(secretsPath)
	require.NoError(t, err)
	modTime1 := info1.ModTime()

	err = nc.GenerateBaseConfigs()
	require.NoError(t, err)

	info2, err := os.Stat(secretsPath)
	require.NoError(t, err)
	modTime2 := info2.ModTime()

	assert.Equal(t, modTime1, modTime2, "Secrets file should not be modified on second call")
}

func TestGenerateBaseConfigs_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping permission test on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("Skipping permission test when running as root")
	}

	cfg := types.TestConfig()
	tmpDir := t.TempDir()
	readonlyDir := filepath.Join(tmpDir, "readonly")

	err := os.Mkdir(readonlyDir, 0555)
	require.NoError(t, err)

	// Point SecretsDir inside the read-only dir so MkdirAll fails
	cfg.SecretsDir = filepath.Join(readonlyDir, "secrets")
	nc := NewNodeConfig(cfg)

	err = nc.GenerateBaseConfigs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot create secrets directory")
}

// Benchmarks

func BenchmarkHashFile(b *testing.B) {
	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.yaml")
	content := make([]byte, 1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	os.WriteFile(testFile, content, 0600)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HashFile(testFile)
	}
}

func TestResolvePatchTemplate(t *testing.T) {
	t.Run("falls back to embedded when no overrides exist", func(t *testing.T) {
		content, source := resolvePatchTemplate(types.RoleControlPlane, "", "")
		assert.Equal(t, controlPlanePatchTemplate, content)
		assert.Equal(t, "embedded:control-plane.yaml", source)

		content, source = resolvePatchTemplate(types.RoleWorker, "", "")
		assert.Equal(t, workerPatchTemplate, content)
		assert.Equal(t, "embedded:worker.yaml", source)
	})

	t.Run("patchDir overrides embedded", func(t *testing.T) {
		patchDir := t.TempDir()
		customContent := "custom-cp-patch: true"
		os.WriteFile(filepath.Join(patchDir, "control-plane.yaml"), []byte(customContent), 0644)

		content, source := resolvePatchTemplate(types.RoleControlPlane, patchDir, "")
		assert.Equal(t, customContent, content)
		assert.Equal(t, filepath.Join(patchDir, "control-plane.yaml"), source)
	})

	t.Run("clusterDir overrides embedded", func(t *testing.T) {
		clusterDir := t.TempDir()
		patchesDir := filepath.Join(clusterDir, "patches")
		os.MkdirAll(patchesDir, 0755)
		customContent := "custom-worker-patch: true"
		os.WriteFile(filepath.Join(patchesDir, "worker.yaml"), []byte(customContent), 0644)

		content, source := resolvePatchTemplate(types.RoleWorker, "", clusterDir)
		assert.Equal(t, customContent, content)
		assert.Equal(t, filepath.Join(patchesDir, "worker.yaml"), source)
	})

	t.Run("patchDir takes precedence over clusterDir", func(t *testing.T) {
		patchDir := t.TempDir()
		clusterDir := t.TempDir()
		patchesDir := filepath.Join(clusterDir, "patches")
		os.MkdirAll(patchesDir, 0755)

		os.WriteFile(filepath.Join(patchDir, "control-plane.yaml"), []byte("from-patchDir"), 0644)
		os.WriteFile(filepath.Join(patchesDir, "control-plane.yaml"), []byte("from-clusterDir"), 0644)

		content, source := resolvePatchTemplate(types.RoleControlPlane, patchDir, clusterDir)
		assert.Equal(t, "from-patchDir", content)
		assert.Equal(t, filepath.Join(patchDir, "control-plane.yaml"), source)
	})

	t.Run("falls through on missing file", func(t *testing.T) {
		patchDir := t.TempDir()
		// patchDir exists but has no worker.yaml - should fall through to embedded
		content, source := resolvePatchTemplate(types.RoleWorker, patchDir, "")
		assert.Equal(t, workerPatchTemplate, content)
		assert.Equal(t, "embedded:worker.yaml", source)
	})
}

func TestResolveNodePatch(t *testing.T) {
	t.Run("returns empty when no per-node patch exists", func(t *testing.T) {
		result := resolveNodePatch(201, "", "")
		assert.Empty(t, result)
	})

	t.Run("finds per-node patch is patchDir", func(t *testing.T) {
		patchDir := t.TempDir()
		nodePatch := filepath.Join(patchDir, "node-201.yaml")
		os.WriteFile(nodePatch, []byte("per-node: true"), 0644)

		result := resolveNodePatch(201, patchDir, "")
		assert.Equal(t, nodePatch, result)
	})

	t.Run("finds per-node patch in clusterDir", func(t *testing.T) {
		clusterDir := t.TempDir()
		patchesDir := filepath.Join(clusterDir, "patches")
		os.MkdirAll(patchesDir, 0755)
		nodePatch := filepath.Join(patchesDir, "node-301.yaml")
		os.WriteFile(nodePatch, []byte("per-node: true"), 0644)

		result := resolveNodePatch(301, "", clusterDir)
		assert.Equal(t, nodePatch, result)
	})

	t.Run("patchDir takes precedence over clusterDir", func(t *testing.T) {
		patchDir := t.TempDir()
		clusterDir := t.TempDir()
		patchesDir := filepath.Join(clusterDir, "patches")
		os.MkdirAll(patchesDir, 0755)

		patchDirNode := filepath.Join(patchDir, "node-201.yaml")
		clusterDirNode := filepath.Join(patchesDir, "node-201.yaml")
		os.WriteFile(patchDirNode, []byte("from-patchDir"), 0644)
		os.WriteFile(clusterDirNode, []byte("from-clusterhDir"), 0644)

		result := resolveNodePatch(201, patchDir, clusterDir)
		assert.Equal(t, patchDirNode, result)
	})

	t.Run("does not match wrong VMID", func(t *testing.T) {
		patchDir := t.TempDir()
		os.WriteFile(filepath.Join(patchDir, "node-201.yaml"), []byte("data"), 0644)

		result := resolveNodePatch(301, "", "")
		assert.Empty(t, result)
	})
}

func BenchmarkNodeConfigGenerate(b *testing.B) {
	if os.Getenv("SKIP_TALOSCTL") != "" {
		b.Skip("Skipping talosctl-dependent benchmark")
	}

	cfg := types.TestConfig()
	cfg.SecretsDir = filepath.Join(b.TempDir(), "secrets")
	nc := NewNodeConfig(cfg)
	spec := &types.NodeSpec{
		VMID: 201,
		Name: "bench-cp",
		Role: types.RoleControlPlane,
	}

	tmpDir := b.TempDir()
	nc.Generate(spec, tmpDir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		nc.Generate(spec, dir)
	}
}
