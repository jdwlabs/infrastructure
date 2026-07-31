package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func loadExtrasFrom(t *testing.T, content string) *types.Config {
	t.Helper()

	cfg := types.DefaultConfig()
	cfg.TerraformTFVars = filepath.Join(t.TempDir(), "terraform.tfvars")
	require.NoError(t, os.WriteFile(cfg.TerraformTFVars, []byte(content), 0o644))

	require.NoError(t, NewManager(cfg, zaptest.NewLogger(t)).LoadTerraformExtras(context.Background()))
	return cfg
}

func TestLoadTerraformExtras_HAProxyVMs(t *testing.T) {
	cfg := loadExtrasFrom(t, `
haproxy_vms = [
  {
    node_name = "pve1"
    vm_name   = "haproxy-1"
    vmid      = 110
    cpu_cores = 2
    memory    = 1024
    disk_size = 10
    ip        = "192.168.1.198/24"
  },
]
`)

	require.Len(t, cfg.HAProxyVMs, 1)
	assert.Equal(t, types.HAProxyVM{
		Name: "haproxy-1",
		Node: "pve1",
		VMID: 110,
		// Stripped so it compares directly against the address the push targets.
		IP: "192.168.1.198",
	}, cfg.HAProxyVMs[0])
}

func TestLoadTerraformExtras_HAProxyVMsListShapeSupportsAPair(t *testing.T) {
	cfg := loadExtrasFrom(t, `
haproxy_vms = [
  { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, ip = "192.168.1.198/24" },
  { node_name = "pve5", vm_name = "haproxy-2", vmid = 111, ip = "192.168.1.197/24" },
]
`)

	require.Len(t, cfg.HAProxyVMs, 2)
	assert.Equal(t, "haproxy-2", cfg.HAProxyVMs[1].Name)
	assert.Equal(t, "pve5", cfg.HAProxyVMs[1].Node)
}

// An empty list is the state of the repo today, and it is what tells the
// command layer that the live load balancer is not reproducible from here.
func TestLoadTerraformExtras_EmptyHAProxyVMsYieldsNoEntries(t *testing.T) {
	cfg := loadExtrasFrom(t, "haproxy_vms = []\nhaproxy_ip = \"192.168.1.199\"\n")

	assert.Empty(t, cfg.HAProxyVMs)
	require.NotNil(t, cfg.HAProxyIP)
	assert.Equal(t, "192.168.1.199", cfg.HAProxyIP.String())
}

func TestLoadTerraformExtras_AbsentHAProxyVMsYieldsNoEntries(t *testing.T) {
	assert.Empty(t, loadExtrasFrom(t, `cluster_name = "core"`).HAProxyVMs)
}

// VMID is the identity the Terraform-state lookup keys on. An entry without one
// would land in the list as VMID 0 and compare equal to every other malformed
// entry.
func TestLoadTerraformExtras_HAProxyVMWithoutVMIDIsSkipped(t *testing.T) {
	cfg := loadExtrasFrom(t, `
haproxy_vms = [
  { node_name = "pve1", vm_name = "haproxy-broken", ip = "192.168.1.198/24" },
  { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, ip = "192.168.1.197/24" },
]
`)

	require.Len(t, cfg.HAProxyVMs, 1)
	assert.Equal(t, "haproxy-1", cfg.HAProxyVMs[0].Name)
}

func TestLoadTerraformExtras_HAProxyVMAddressWithoutCIDRIsKeptAsIs(t *testing.T) {
	cfg := loadExtrasFrom(t, `
haproxy_vms = [
  { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, ip = "192.168.1.198" },
]
`)

	require.Len(t, cfg.HAProxyVMs, 1)
	assert.Equal(t, "192.168.1.198", cfg.HAProxyVMs[0].IP)
}
