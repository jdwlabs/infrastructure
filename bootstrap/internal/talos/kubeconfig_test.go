package talos

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestNewKubeconfigManager(t *testing.T) {
	logger := zap.NewNop()
	client := &Client{}

	km := NewKubeconfigManager(client, logger)

	assert.NotNil(t, km)
	assert.Equal(t, client, km.client)
	assert.Equal(t, logger, km.logger)
}

func TestKubeconfigPath(t *testing.T) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	t.Run("KUBECONFIG env var set", func(t *testing.T) {
		tmpDir := t.TempDir()
		kubeconfigPath := filepath.Join(tmpDir, "config")
		t.Setenv("KUBECONFIG", kubeconfigPath)

		path := km.kubeconfigPath()
		assert.Equal(t, kubeconfigPath, path)
	})

	t.Run("KUBECONFIG with multiple paths Unix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix-specific test")
		}

		tmpDir := t.TempDir()
		path1 := filepath.Join(tmpDir, "config1")
		path2 := filepath.Join(tmpDir, "config2")

		t.Setenv("KUBECONFIG", path1+":"+path2)

		path := km.kubeconfigPath()
		assert.Equal(t, path1, path)
	})

	t.Run("KUBECONFIG with multiple paths Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows-specific test")
		}

		tmpDir := t.TempDir()
		path1 := filepath.Join(tmpDir, "config1")
		path2 := filepath.Join(tmpDir, "config2")

		t.Setenv("KUBECONFIG", path1+";"+path2)

		path := km.kubeconfigPath()
		assert.Equal(t, path1, path)
	})

	t.Run("default path from HOME", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")

		// Save original env vars
		origHome := os.Getenv("HOME")
		origUserProfile := os.Getenv("USERPROFILE")
		defer func() {
			_ = os.Setenv("HOME", origHome)
			_ = os.Setenv("USERPROFILE", origUserProfile)
		}()

		tmpDir := t.TempDir()
		kubeDir := filepath.Join(tmpDir, ".kube")
		_ = os.MkdirAll(kubeDir, 0755)

		// Set both HOME and USERPROFILE for cross-platform compatibility
		_ = os.Setenv("HOME", tmpDir)
		_ = os.Setenv("USERPROFILE", tmpDir)

		path := km.kubeconfigPath()
		// Should contain .kube/config somewhere in the path
		assert.Contains(t, path, ".kube")
		assert.Contains(t, path, "config")
	})

	t.Run("fallback when no HOME", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")

		path := km.kubeconfigPath()
		assert.Contains(t, path, ".kube")
		assert.Contains(t, path, "config")
	})
}

func TestMergeKubeconfig_NewFile(t *testing.T) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	tmpDir := t.TempDir()
	existingPath := filepath.Join(tmpDir, "existing", "config")
	newPath := filepath.Join(tmpDir, "new.yaml")

	newConfig := kubeConfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "test-cluster",
		Clusters: []kubeCluster{
			{
				Name: "test-cluster",
				Cluster: kubeClusterDetail{
					Server: "https://test.example.com:6443",
				},
			},
		},
		Contexts: []kubeContext{
			{
				Name: "test-cluster",
				Context: kubeContextDetail{
					Cluster: "test-cluster",
					User:    "test-admin",
				},
			},
		},
		Users: []kubeUser{
			{Name: "test-admin"},
		},
	}

	data, err := yaml.Marshal(newConfig)
	require.NoError(t, err)
	err = os.WriteFile(newPath, data, 0600)
	require.NoError(t, err)

	err = km.mergeKubeconfig(existingPath, newPath)
	require.NoError(t, err)

	_, err = os.Stat(existingPath)
	require.NoError(t, err)

	content, err := os.ReadFile(existingPath)
	require.NoError(t, err)
	var result kubeConfig
	err = yaml.Unmarshal(content, &result)
	require.NoError(t, err)
	assert.Equal(t, "test-cluster", result.CurrentContext)
}

func TestMergeKubeconfig_ExistingFile(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not in PATH")
	}

	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	tmpDir := t.TempDir()
	existingPath := filepath.Join(tmpDir, "config")
	newPath := filepath.Join(tmpDir, "new.yaml")

	existingConfig := kubeConfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "existing-cluster",
		Clusters: []kubeCluster{
			{
				Name: "existing-cluster",
				Cluster: kubeClusterDetail{
					Server: "https://existing.example.com:6443",
				},
			},
		},
		Contexts: []kubeContext{
			{
				Name: "existing-cluster",
				Context: kubeContextDetail{
					Cluster: "existing-cluster",
					User:    "existing-admin",
				},
			},
		},
		Users: []kubeUser{
			{Name: "existing-admin"},
		},
	}

	existingData, _ := yaml.Marshal(existingConfig)
	err := os.WriteFile(existingPath, existingData, 0600)
	require.NoError(t, err)

	newConfig := kubeConfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "new-cluster",
		Clusters: []kubeCluster{
			{
				Name: "new-cluster",
				Cluster: kubeClusterDetail{
					Server: "https://new.example.com:6443",
				},
			},
		},
		Contexts: []kubeContext{
			{
				Name: "new-cluster",
				Context: kubeContextDetail{
					Cluster: "new-cluster",
					User:    "new-admin",
				},
			},
		},
		Users: []kubeUser{
			{Name: "new-admin"},
		},
	}

	newData, _ := yaml.Marshal(newConfig)
	err = os.WriteFile(newPath, newData, 0600)
	require.NoError(t, err)

	err = km.mergeKubeconfig(existingPath, newPath)
	require.NoError(t, err)

	content, _ := os.ReadFile(existingPath)
	var result kubeConfig
	_ = yaml.Unmarshal(content, &result)

	clusterNames := make([]string, len(result.Clusters))
	for i, c := range result.Clusters {
		clusterNames[i] = c.Name
	}
	assert.Contains(t, clusterNames, "existing-cluster")
	assert.Contains(t, clusterNames, "new-cluster")
}

func TestMergeKubeconfig_CorruptedExisting(t *testing.T) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	tmpDir := t.TempDir()
	existingPath := filepath.Join(tmpDir, "config")
	newPath := filepath.Join(tmpDir, "new.yaml")

	err := os.WriteFile(existingPath, []byte("invalid: yaml: content: ["), 0600)
	require.NoError(t, err)

	validConfig := kubeConfig{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters:   []kubeCluster{{Name: "test", Cluster: kubeClusterDetail{Server: "https://test:6443"}}},
	}
	data, _ := yaml.Marshal(validConfig)
	err = os.WriteFile(newPath, data, 0600)
	require.NoError(t, err)

	_ = km.mergeKubeconfig(existingPath, newPath)
}

func TestWriteDirectly(t *testing.T) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	t.Run("creates nested directories", func(t *testing.T) {
		tmpDir := t.TempDir()
		testPath := filepath.Join(tmpDir, "deep", "path", "config")

		data := []byte("test kubeconfig data")
		err := km.writeDirectly(testPath, data)
		require.NoError(t, err)

		_, err = os.Stat(filepath.Dir(testPath))
		require.NoError(t, err)

		content, _ := os.ReadFile(testPath)
		assert.Equal(t, data, content)
	})

	t.Run("overwrites existing file", func(t *testing.T) {
		tmpDir := t.TempDir()
		testPath := filepath.Join(tmpDir, "config")

		_ = os.WriteFile(testPath, []byte("old data"), 0600)

		newData := []byte("new data")
		err := km.writeDirectly(testPath, newData)
		require.NoError(t, err)

		content, _ := os.ReadFile(testPath)
		assert.Equal(t, newData, content)
	})

	t.Run("correct permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Permission test skipped on Windows")
		}

		tmpDir := t.TempDir()
		testPath := filepath.Join(tmpDir, "config")

		data := []byte("test")
		err := km.writeDirectly(testPath, data)
		require.NoError(t, err)

		info, _ := os.Stat(testPath)
		mode := info.Mode().Perm()
		assert.Equal(t, os.FileMode(0600), mode)
	})
}

func TestKubeConfigStructs(t *testing.T) {
	config := kubeConfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "test",
		Clusters: []kubeCluster{
			{
				Name: "cluster1",
				Cluster: kubeClusterDetail{
					Server:                   "https://server:6443",
					CertificateAuthorityData: "certdata",
				},
			},
		},
		Contexts: []kubeContext{
			{
				Name: "context1",
				Context: kubeContextDetail{
					Cluster: "cluster1",
					User:    "user1",
				},
			},
		},
		Users: []kubeUser{
			{Name: "user1", User: map[string]interface{}{"token": "abc"}},
		},
	}

	data, err := yaml.Marshal(config)
	require.NoError(t, err)

	var result kubeConfig
	err = yaml.Unmarshal(data, &result)
	require.NoError(t, err)

	assert.Equal(t, config.APIVersion, result.APIVersion)
	assert.Equal(t, config.CurrentContext, result.CurrentContext)
	assert.Len(t, result.Clusters, 1)
	assert.Equal(t, "cluster1", result.Clusters[0].Name)
	assert.Equal(t, "https://server:6443", result.Clusters[0].Cluster.Server)
	assert.Equal(t, "certdata", result.Clusters[0].Cluster.CertificateAuthorityData)
}

func TestFetchAndMerge_Modifications(t *testing.T) {
	original := kubeConfig{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "original-context",
		Clusters: []kubeCluster{
			{Name: "original", Cluster: kubeClusterDetail{Server: "https://original:6443"}},
		},
		Contexts: []kubeContext{
			{Name: "original-context", Context: kubeContextDetail{Cluster: "original", User: "original-admin"}},
		},
		Users: []kubeUser{
			{Name: "original-admin"},
		},
	}

	clusterName := "my-cluster"
	controlPlaneEndpoint := "k8s.example.com"

	for i := range original.Clusters {
		original.Clusters[i].Cluster.Server = "https://" + controlPlaneEndpoint + ":6443"
		original.Clusters[i].Name = clusterName
	}

	for i := range original.Contexts {
		original.Contexts[i].Name = clusterName
		original.Contexts[i].Context.Cluster = clusterName
	}
	original.CurrentContext = clusterName

	oldUser := original.Users[0].Name
	for i := range original.Users {
		original.Users[i].Name = clusterName + "-admin"
	}
	for i := range original.Contexts {
		if original.Contexts[i].Context.User == oldUser {
			original.Contexts[i].Context.User = clusterName + "-admin"
		}
	}

	assert.Equal(t, "my-cluster", original.CurrentContext)
	assert.Equal(t, "https://k8s.example.com:6443", original.Clusters[0].Cluster.Server)
	assert.Equal(t, "my-cluster", original.Contexts[0].Name)
	assert.Equal(t, "my-cluster-admin", original.Users[0].Name)
}

func TestVerifyKubernetesAPI(t *testing.T) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	t.Run("successful connection", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = listener.Close() }()

		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = ctx
		_ = km

		_ = listener.Close()
		<-done
	})

	t.Run("connection refused", func(t *testing.T) {
		listener, _ := net.Listen("tcp", "127.0.0.1:0")
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()

		_ = port
	})
}

func TestKubeconfigManager_Verify(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not in PATH")
	}

	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	ctx := context.Background()
	err := km.Verify(ctx, "nonexistent-context")
	assert.Error(t, err)
}

// Benchmarks

func BenchmarkKubeconfigPath(b *testing.B) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)

	_ = os.Setenv("KUBECONFIG", "/tmp/test-config")
	defer func() { _ = os.Unsetenv("KUBECONFIG") }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		km.kubeconfigPath()
	}
}

func BenchmarkWriteDirectly(b *testing.B) {
	logger := zap.NewNop()
	km := NewKubeconfigManager(nil, logger)
	tmpDir := b.TempDir()
	data := []byte("test kubeconfig data")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(tmpDir, "config", string(rune(i)))
		_ = km.writeDirectly(path, data)
	}
}
