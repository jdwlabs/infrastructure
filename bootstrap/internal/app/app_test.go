package app

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitConfigSecretsDir(t *testing.T) {
	t.Run("derives SecretsDir from ClusterName set by flag", func(t *testing.T) {
		a := New("test")
		// Cobra writes the --cluster flag value directly into the struct
		// before PersistentPreRunE runs; simulate that here.
		a.Cfg.ClusterName = "core"

		require.NoError(t, a.InitConfig(nil))

		assert.Equal(t, filepath.Join("clusters", "core", "secrets"), a.Cfg.SecretsDir)
	})

	t.Run("CLUSTER_NAME env overrides flag value", func(t *testing.T) {
		a := New("test")
		a.Cfg.ClusterName = "core"
		t.Setenv("CLUSTER_NAME", "alt")

		require.NoError(t, a.InitConfig(nil))

		assert.Equal(t, "alt", a.Cfg.ClusterName)
		assert.Equal(t, filepath.Join("clusters", "alt", "secrets"), a.Cfg.SecretsDir)
	})

	t.Run("SECRETS_DIR env wins over derived path", func(t *testing.T) {
		a := New("test")
		a.Cfg.ClusterName = "core"
		t.Setenv("SECRETS_DIR", filepath.Join("custom", "secrets"))

		require.NoError(t, a.InitConfig(nil))

		assert.Equal(t, filepath.Join("custom", "secrets"), a.Cfg.SecretsDir)
	})

	t.Run("default cluster name keeps default SecretsDir", func(t *testing.T) {
		a := New("test")

		require.NoError(t, a.InitConfig(nil))

		assert.Equal(t, filepath.Join("clusters", "cluster", "secrets"), a.Cfg.SecretsDir)
	})
}
