package kubectl

import (
	"context"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetConfigMapData(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	const payload = `{"_kubelet-serving-cert-approver__Namespace":"sha256:aaa"}`

	var gotArgs []string
	execCommandContext = func(_ context.Context, _ string, arg ...string) *exec.Cmd {
		gotArgs = arg
		return mockExecCmd(payload, 0)
	}

	client, _ := newTestClient(t)
	out, err := client.GetConfigMapData(context.Background(), "kube-system", "talos-bootstrap-manifests-inventory")

	require.NoError(t, err)
	assert.JSONEq(t, payload, string(out))
	assert.Equal(t, []string{
		"-n", "kube-system", "get", "cm", "talos-bootstrap-manifests-inventory",
		"-o", "jsonpath={.data}",
	}, gotArgs)
}

func TestGetConfigMapDataErrorIncludesCommand(t *testing.T) {
	originalExec := execCommandContext
	defer func() { execCommandContext = originalExec }()

	execCommandContext = func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit", "1")
		}
		return exec.Command("false")
	}

	client, _ := newTestClient(t)
	_, err := client.GetConfigMapData(context.Background(), "kube-system", "missing-cm")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "get cm missing-cm")
}
