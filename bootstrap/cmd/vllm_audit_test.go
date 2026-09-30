package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/app"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRepo builds a throwaway checkout holding the committed serving.yaml
// and audit.yaml, an encrypted tfvars vault file, and a logging fake sops
// first on an otherwise empty PATH, then runs from inside it.
func auditRepo(t *testing.T) (repo, sopsLog string) {
	t.Helper()
	serving, err := os.ReadFile("../../inference/vllm/serving.yaml")
	require.NoError(t, err)
	audit, err := os.ReadFile("../../inference/vllm/audit.yaml")
	require.NoError(t, err)

	repo = t.TempDir()
	for _, d := range []string{".git", "terraform", "inference/vllm", "bin"} {
		require.NoError(t, os.MkdirAll(filepath.Join(repo, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "terraform/terraform.tfvars.enc.yaml"), []byte("sops: {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "inference/vllm/serving.yaml"), serving, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "inference/vllm/audit.yaml"), audit, 0o644))

	sopsLog = filepath.Join(repo, "sops-calls.log")
	fake := "#!/bin/sh\necho \"$@\" >> " + sopsLog + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "bin/sops"), []byte(fake), 0o755))

	t.Setenv("PATH", filepath.Join(repo, "bin"))
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"HF_TOKEN", "SOPS_AGE_KEY", "SOPS_AGE_KEY_FILE", "JIRA_API_TOKEN", "CLUSTER_NAME", "SECRETS_DIR"} {
		t.Setenv(k, "")
	}
	t.Chdir(repo)
	return repo, sopsLog
}

func useFakeHub(t *testing.T, h *audittest.Hub) {
	t.Helper()
	prev := auditEndpointsFor
	auditEndpointsFor = func() app.AuditEndpoints {
		return app.AuditEndpoints{
			HubBase: h.Server.URL, RawBase: h.Server.URL, HTTP: h.Server.Client(),
			Sleep: func(context.Context, time.Duration) error { return nil },
		}
	}
	t.Cleanup(func() { auditEndpointsFor = prev })
}

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	a := app.New("test")
	a.Cfg.NoColor = true
	out, err := execute(t, newRootCmd(a), args...)
	a.Close(err)
	return out, err
}

// The audit reads public APIs only. With sops on PATH and no age key, the
// root hook's hydrate would exit 1; the audit's own hook must never reach it,
// nor even exec sops to log its version.
func TestVLLMAuditNeverTouchesTheVault(t *testing.T) {
	repo, sopsLog := auditRepo(t)
	useFakeHub(t, audittest.NewHub(t))

	out, err := runRoot(t, "vllm", "audit", "--dry-run")

	require.NoError(t, err, out)
	assert.Equal(t, 0, ExitCode(err))
	assert.NoFileExists(t, sopsLog, "sops must never be invoked, not even sops version")
	assert.NoFileExists(t, filepath.Join(repo, "terraform/terraform.tfvars"), "no plaintext vault file")
	assert.Contains(t, out, "summary:")
}

func TestVLLMAuditWithoutDryRunRefusesBeforeAnyRequest(t *testing.T) {
	auditRepo(t)
	h := audittest.NewHub(t)
	useFakeHub(t, h)

	out, err := runRoot(t, "vllm", "audit")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "jira_unconfigured")
	assert.Contains(t, out, "JIRA_API_TOKEN unset")
	assert.Empty(t, h.Requests())
}

func TestVLLMAuditJSONIsOneObject(t *testing.T) {
	auditRepo(t)
	useFakeHub(t, audittest.NewHub(t))

	out, err := runRoot(t, "vllm", "audit", "-d", "--json", "--host", "10.0.0.1")

	require.NoError(t, err, out)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &obj), "stdout must be exactly one JSON object")
	assert.Equal(t, "audit", obj["event"])
}

func TestVLLMAuditHelpDescribesItsOwnFlags(t *testing.T) {
	out, err := execute(t, newRootCmd(app.New("test")), "vllm", "audit", "--help")

	require.NoError(t, err)
	assert.Contains(t, out, "Emit the report as one JSON object")
	assert.NotContains(t, out, "one object per state transition")
	assert.Contains(t, out, "Ignored: the audit never contacts the GPU host")
	assert.Contains(t, out, "--dry-run")
	assert.Contains(t, out, "talops vllm audit --dry-run")
}

func TestVLLMAuditUnknownFlagIsStructuredAndExitsOne(t *testing.T) {
	out, err := execute(t, newRootCmd(app.New("test")), "vllm", "audit", "--bogus")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "error: {code: unknown_flag")
}
