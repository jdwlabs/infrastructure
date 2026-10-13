package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAgeKey(t *testing.T, dir, name string) (keyFile, pub string) {
	t.Helper()
	keyFile = filepath.Join(dir, name)
	out, err := exec.Command("age-keygen", "-o", keyFile).CombinedOutput()
	require.NoError(t, err, "age-keygen: %s", out)
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "Public key: "); ok {
			return keyFile, strings.TrimSpace(p)
		}
	}
	t.Fatal("age-keygen printed no public key")
	return "", ""
}

// vaultRepo builds a throwaway checkout whose tfvars vault file is really
// encrypted to one fresh age key, then runs from inside it.
func vaultRepo(t *testing.T) (repo, sopsYAML, encPath string) {
	t.Helper()
	for _, bin := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	repo = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "terraform"), 0o755))

	keyFile, pub := newAgeKey(t, t.TempDir(), "key.txt")
	sopsYAML = filepath.Join(repo, ".sops.yaml")
	require.NoError(t, os.WriteFile(sopsYAML,
		[]byte("creation_rules:\n  - path_regex: \\.enc\\.ya?ml$\n    age: "+pub+"\n"), 0o644))

	plain := filepath.Join(repo, "terraform/plain.tmp")
	require.NoError(t, os.WriteFile(plain, []byte("token = \"fixture-value\"\n"), 0o600))
	encPath = filepath.Join(repo, "terraform/terraform.tfvars.enc.yaml")
	out, err := exec.Command("sops", "--config", sopsYAML, "encrypt", "--input-type", "binary", "--output-type", "yaml",
		"--filename-override", encPath, "--output", encPath, plain).CombinedOutput()
	require.NoError(t, err, "sops encrypt: %s", out)
	require.NoError(t, os.Remove(plain))

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	for _, k := range []string{"SOPS_AGE_KEY", "CLUSTER_NAME", "SECRETS_DIR"} {
		t.Setenv(k, "")
	}
	t.Chdir(repo)
	return repo, sopsYAML, encPath
}

func TestSecretsAddDeviceDryRunWritesNothing(t *testing.T) {
	repo, sopsYAML, encPath := vaultRepo(t)
	_, newPub := newAgeKey(t, t.TempDir(), "new.txt")
	beforeCfg, err := os.ReadFile(sopsYAML)
	require.NoError(t, err)
	beforeEnc, err := os.ReadFile(encPath)
	require.NoError(t, err)

	out, err := runRoot(t, "secrets", "add-device", "--dry-run", newPub)

	require.NoError(t, err, out)
	afterCfg, err := os.ReadFile(sopsYAML)
	require.NoError(t, err)
	afterEnc, err := os.ReadFile(encPath)
	require.NoError(t, err)
	assert.Equal(t, string(beforeCfg), string(afterCfg), ".sops.yaml must not change")
	assert.Equal(t, string(beforeEnc), string(afterEnc), "vault file must not be re-keyed")
	assert.NoFileExists(t, filepath.Join(repo, "terraform/terraform.tfvars"), "no plaintext hydration")
	assert.Contains(t, out, newPub)
	assert.Contains(t, out, "terraform/terraform.tfvars.enc.yaml")
	assert.NotContains(t, out, "device added")
}

func TestSecretsAddDeviceWithoutDryRunRekeys(t *testing.T) {
	_, sopsYAML, encPath := vaultRepo(t)
	_, newPub := newAgeKey(t, t.TempDir(), "new.txt")
	beforeEnc, err := os.ReadFile(encPath)
	require.NoError(t, err)

	out, err := runRoot(t, "secrets", "add-device", newPub)

	require.NoError(t, err, out)
	cfg, err := os.ReadFile(sopsYAML)
	require.NoError(t, err)
	assert.Contains(t, string(cfg), newPub)
	afterEnc, err := os.ReadFile(encPath)
	require.NoError(t, err)
	assert.NotEqual(t, string(beforeEnc), string(afterEnc))
}
