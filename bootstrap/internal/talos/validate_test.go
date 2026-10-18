package talos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/require"
)

// targetPinsFile is the committed example tfvars, the documented source of the
// versions a cluster is built against.
const targetPinsFile = "../../../terraform/terraform.tfvars.example"

// targetPin extracts a top-level `name = "value"` string from the example
// tfvars. Deliberately not the full HCL parser: only flat string pins are read
// here, and a miss must fail the caller rather than silently default.
func targetPin(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(targetPinsFile)
	require.NoError(t, err, "read version pins")

	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`).FindSubmatch(content)
	require.NotNil(t, m, "%s not found in %s", name, targetPinsFile)

	return string(m[1])
}

// TestRolePatchesValidateAgainstTargetVersion renders each role's patch the way
// talops renders it for a real node and runs the Talos validator over the
// result, at the Talos version this repo currently targets and at the version the live
// cluster still runs.
//
// Rendering is what makes the check honest. A patch key can be individually
// well-formed and still be rejected once it lands on a base config, because
// validation runs over the merged document and talosctl fills in defaults that
// the patch has to coexist with. Reading the patch files alone — or diffing
// them against a schema — cannot see that class of conflict, and neither can
// `machineconfig patch`, which merges without validating. It surfaces at
// `apply-config` time, on a node, mid-rollout.
//
// The pins come from the tfvars example rather than being restated here so a
// version bump is validated against the version it bumps to.
//
// Unlike the other talosctl-dependent tests in this package this one ignores
// SKIP_TALOSCTL: an opt-out that turns a correctness gate into a no-op is worth
// less than the gate. It skips only when talosctl is genuinely absent, which CI
// rules out by installing a pinned talosctl and running it before the suite.
func TestRolePatchesValidateAgainstTargetVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping talosctl-dependent test on Windows - patch paths use Unix conventions")
	}

	talosctl, err := exec.LookPath("talosctl")
	if err != nil {
		t.Skipf("talosctl not on PATH: %v", err)
	}

	// The repo target is what a fresh cluster is built from. The live-cluster
	// contract is the last release whose nodes only understand the v1alpha1
	// install/kubernetes fields; it must keep rendering a config those nodes
	// accept until they are upgraded. Both are validated by the one pinned
	// talosctl, which is the newer of the two and so judges the older contract
	// by the rules the upgraded nodes will apply.
	targets := []struct {
		name           string
		talosVersion   string
		installerImage string
	}{
		{"live-cluster", "v1.13.4", "ghcr.io/siderolabs/installer:v1.13.4"},
		{"repo-target", targetPin(t, "talos_version"), targetPin(t, "installer_image")},
	}

	specs := []*types.NodeSpec{
		{VMID: 201, Name: "talos-cp-1", Node: "pve1", CPU: 4, Memory: 8192, Disk: 50, Role: types.RoleControlPlane},
		{VMID: 301, Name: "talos-worker-1", Node: "pve2", CPU: 8, Memory: 16384, Disk: 100, Role: types.RoleWorker},
	}

	for _, target := range targets {
		cfg := types.TestConfig()
		cfg.KubernetesVersion = targetPin(t, "kubernetes_version")
		cfg.TalosVersion = target.talosVersion
		cfg.InstallerImage = target.installerImage
		cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")

		nc := NewNodeConfig(cfg)

		for _, spec := range specs {
			t.Run(target.name+"/"+string(spec.Role), func(t *testing.T) {
				outputDir := t.TempDir()

				_, err := nc.Generate(spec, outputDir)
				require.NoError(t, err, "render %s config", spec.Role)

				rendered := filepath.Join(outputDir, fmt.Sprintf("node-%s-%d.yaml", spec.Role, spec.VMID))

				// metal is the mode the nodes actually install in, and the only one
				// that exercises the install-section rules at all.
				out, err := exec.Command(talosctl, "validate", "--mode", "metal", "--config", rendered).CombinedOutput()
				require.NoErrorf(t, err, "talosctl validate --mode metal (talos %s):\n%s", cfg.TalosVersion, out)
			})
		}
	}
}
