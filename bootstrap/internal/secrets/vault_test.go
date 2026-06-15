package secrets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// genAgeKey creates an age keypair file and returns (keyFile, publicKey).
func genAgeKey(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	keyFile := filepath.Join(dir, name)
	out, err := exec.Command("age-keygen", "-o", keyFile).CombinedOutput()
	require.NoError(t, err, "age-keygen: %s", out)
	data, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# public key:") {
			return keyFile, strings.TrimSpace(strings.TrimPrefix(line, "# public key:"))
		}
	}
	t.Fatalf("no public key in %s", keyFile)
	return "", ""
}

// setupVault creates an isolated repo-like working dir with a .sops.yaml and an
// age key, chdirs into it, and returns a Vault plus the primary key file.
func setupVault(t *testing.T) (*Vault, string) {
	t.Helper()
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops not installed")
	}
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen not installed")
	}

	root := t.TempDir()
	keyFile, pub := genAgeKey(t, root, "key.txt")
	sopsYAML := "creation_rules:\n  - path_regex: \\.enc\\.ya?ml$\n    age: " + pub + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".sops.yaml"), []byte(sopsYAML), 0644))

	t.Chdir(root)
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	cfg := types.TestConfig()
	cfg.ClusterName = "test"
	cfg.SecretsDir = filepath.Join("clusters", "test", "secrets")
	cfg.TerraformTFVars = filepath.Join("terraform", "terraform.tfvars")

	return New(cfg, zaptest.NewLogger(t)), keyFile
}

// seedPlaintext writes representative plaintext working files.
func seedPlaintext(t *testing.T, v *Vault) map[string][]byte {
	t.Helper()
	content := map[string][]byte{
		v.TFVarsEntry().Plain: []byte("proxmox_api_token_secret = \"s3cr3t-token\"\ncluster_name = \"test\"\n"),
	}
	ce := v.ClusterEntries()
	content[ce[0].Plain] = []byte("secrets:\n  bootstrap-token: AAAA-crown-jewel\n")
	content[ce[1].Plain] = []byte("context: test\n")
	content[ce[2].Plain] = []byte("{\"cluster_name\":\"test\",\"bootstrap_completed\":true}\n")
	for path, data := range content {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, data, 0600))
	}
	return content
}

func TestSealHydrateRoundTrip(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	want := seedPlaintext(t, v)

	sealed, err := v.Seal(ctx)
	require.NoError(t, err)
	require.Len(t, sealed, len(v.Entries()))

	// Encrypted files exist and contain no plaintext secret material.
	for _, e := range v.Entries() {
		enc, err := os.ReadFile(e.Enc)
		require.NoError(t, err, "vault file %s should exist", e.Enc)
		require.NotContains(t, string(enc), "crown-jewel")
		require.NotContains(t, string(enc), "s3cr3t-token")
		require.Contains(t, string(enc), "ENC[")
	}

	// Wipe plaintext, then hydrate restores byte-for-byte.
	for p := range want {
		require.NoError(t, os.Remove(p))
	}
	hydrated, err := v.Hydrate(ctx)
	require.NoError(t, err)
	require.Len(t, hydrated, len(v.Entries()))
	for p, data := range want {
		got, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
}

func TestSealIsIdempotent(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	seedPlaintext(t, v)

	_, err := v.Seal(ctx)
	require.NoError(t, err)

	// Unchanged plaintext should produce no re-encryption (keeps git clean).
	sealed, err := v.Seal(ctx)
	require.NoError(t, err)
	require.Empty(t, sealed)
}

func TestLockRemovesPlaintext(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	seedPlaintext(t, v)

	require.NoError(t, v.Lock(ctx))
	for _, e := range v.Entries() {
		_, err := os.Stat(e.Plain)
		require.True(t, os.IsNotExist(err), "plaintext %s should be gone", e.Plain)
		_, err = os.Stat(e.Enc)
		require.NoError(t, err, "vault %s should remain", e.Enc)
	}
}

func TestAddRecipient(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	seedPlaintext(t, v)
	_, err := v.Seal(ctx)
	require.NoError(t, err)

	key2, pub2 := genAgeKey(t, t.TempDir(), "key2.txt")
	require.NoError(t, v.AddRecipient(ctx, pub2))

	recips, err := Recipients()
	require.NoError(t, err)
	require.Contains(t, recips, pub2)

	// The second key alone can now decrypt a vault file.
	t.Setenv("SOPS_AGE_KEY_FILE", key2)
	got, err := v.decrypt(ctx, v.TFVarsEntry().Enc)
	require.NoError(t, err)
	require.Contains(t, string(got), "s3cr3t-token")
}

func TestAddRecipientBootstrapsConfig(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	// Remove the pre-seeded config so add-device must create it.
	require.NoError(t, os.Remove(SopsConfigPath()))

	_, pub := genAgeKey(t, t.TempDir(), "new.txt")
	require.NoError(t, v.AddRecipient(ctx, pub))

	recips, err := Recipients()
	require.NoError(t, err)
	require.Equal(t, []string{pub}, recips)

	// The freshly created config can now encrypt and decrypt.
	seedPlaintext(t, v)
	_, err = v.Seal(ctx)
	require.NoError(t, err)
}

func TestHydrateSkipsNewerPlaintext(t *testing.T) {
	v, _ := setupVault(t)
	ctx := context.Background()
	seedPlaintext(t, v)
	_, err := v.Seal(ctx)
	require.NoError(t, err)

	// Simulate a local unsealed edit (plaintext newer than vault).
	tfvars := v.TFVarsEntry().Plain
	require.NoError(t, os.WriteFile(tfvars, []byte("local_edit = true\n"), 0600))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(tfvars, future, future))

	_, err = v.Hydrate(ctx)
	require.NoError(t, err)
	got, err := os.ReadFile(tfvars)
	require.NoError(t, err)
	require.Equal(t, "local_edit = true\n", string(got), "hydrate must not clobber newer local edits")
}

func TestKeyAvailable(t *testing.T) {
	v, keyFile := setupVault(t)
	require.NoError(t, v.KeyAvailable())

	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	require.Error(t, v.KeyAvailable())
	_ = keyFile
}
