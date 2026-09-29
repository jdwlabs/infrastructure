package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	committedSpec  = "../../../inference/vllm/serving.yaml"
	committedAudit = "../../../inference/vllm/audit.yaml"
)

func auditOpts(h *audittest.Hub, out *bytes.Buffer, env map[string]string) AuditOptions {
	return AuditOptions{
		Spec:   committedSpec,
		Config: committedAudit,
		DryRun: true,
		Out:    out,
		Endpoints: AuditEndpoints{
			HubBase: h.Server.URL, RawBase: h.Server.URL, HTTP: h.Server.Client(),
			Sleep: func(context.Context, time.Duration) error { return nil },
		},
		Getenv: func(k string) string { return env[k] },
		Now:    func() time.Time { return audittest.Now },
	}
}

func newModel(id string) audittest.Model {
	return audittest.Model{ID: id, SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: audittest.Now.Add(-48 * time.Hour), Downloads: 10,
		PipelineTag: "text-generation", License: "apache-2.0"}
}

func TestRunVLLMAuditDryRunPrintsTheReport(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{newModel("Qwen/Qwen3-Coder-Next-Instruct")}}
	var out bytes.Buffer

	err := New("test").RunVLLMAudit(context.Background(), auditOpts(h, &out, nil))

	require.NoError(t, err)
	assert.Contains(t, out.String(), "candidates[1]{")
	assert.Contains(t, out.String(), "Qwen/Qwen3-Coder-Next-Instruct,allow-listed,")
	assert.Contains(t, out.String(), "jira,dedupe skipped: Jira reads are not built yet")
	assert.Contains(t, out.String(), "  action: not-filed\n")
}

func TestRunVLLMAuditPassesHFTokenToTheHub(t *testing.T) {
	h := audittest.NewHub(t)
	var out bytes.Buffer

	err := New("test").RunVLLMAudit(context.Background(), auditOpts(h, &out, map[string]string{"HF_TOKEN": "hf_x"}))

	require.NoError(t, err)
	assert.Contains(t, h.AuthHeaders(), "Bearer hf_x")
}

func TestRunVLLMAuditRefusesUnreadableInputsBeforeAnyRequest(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("windowDays: 7\nunknownKey: 1\n"), 0o644))
	cases := map[string]struct {
		mut  func(*AuditOptions)
		code string
	}{
		"spec missing":   {func(o *AuditOptions) { o.Spec = filepath.Join(t.TempDir(), "none.yaml") }, "spec_unreadable"},
		"config invalid": {func(o *AuditOptions) { o.Config = bad }, "config_unreadable"},
		"not a dry run":  {func(o *AuditOptions) { o.DryRun = false }, "jira_not_built"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := audittest.NewHub(t)
			var out bytes.Buffer
			opts := auditOpts(h, &out, nil)
			c.mut(&opts)

			err := New("test").RunVLLMAudit(context.Background(), opts)

			var f *modelaudit.Failure
			require.ErrorAs(t, err, &f)
			assert.Equal(t, c.code, f.Code)
			assert.Contains(t, out.String(), "error: {code: "+c.code)
			assert.Empty(t, h.Requests())
		})
	}
}
