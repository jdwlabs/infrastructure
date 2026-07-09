package app

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkReadOnly(t *testing.T) {
	mk := func(name, parent string) *cobra.Command {
		c := &cobra.Command{Use: name}
		if parent != "" {
			p := &cobra.Command{Use: parent}
			p.AddCommand(c)
		}
		return c
	}
	cases := []struct {
		name, parent string
		readOnly     bool
	}{
		{"status", "", true},
		{"plan", "infra", true},
		{"version", "", true},
		{"hydrate", "secrets", true},
		{"seal", "secrets", true},
		{"reconcile", "", false},
		{"up", "", false},
		{"deploy", "infra", false},
	}
	for _, tc := range cases {
		a := New("test")
		a.MarkReadOnly(mk(tc.name, tc.parent))
		assert.Equal(t, tc.readOnly, a.readOnlyCmd, "%s (parent %q)", tc.name, tc.parent)
	}
}

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

func TestInitConfigAdminAllowedCIDRs(t *testing.T) {
	t.Run("unset env leaves AdminAllowedCIDRs untouched", func(t *testing.T) {
		a := New("test")

		require.NoError(t, a.InitConfig(nil))

		assert.Nil(t, a.Cfg.AdminAllowedCIDRs)
	})

	t.Run("ADMIN_ALLOWED_CIDRS env is split and whitespace-trimmed", func(t *testing.T) {
		a := New("test")
		t.Setenv("ADMIN_ALLOWED_CIDRS", "10.0.0.0/8, 192.168.1.0/24 ,203.0.113.5/32")

		require.NoError(t, a.InitConfig(nil))

		assert.Equal(t, []string{"10.0.0.0/8", "192.168.1.0/24", "203.0.113.5/32"}, a.Cfg.AdminAllowedCIDRs)
	})
}
