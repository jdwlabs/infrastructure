package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every case below is decided before any SSH connection or git command, so
// these run with no GPU VM, no serving.yaml, and no network.

func TestVLLMRejectsUnknownFlagAsStructuredOutput(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "status", "--stat", "up")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "error: {code: unknown_flag")
	assert.Contains(t, out, "--stat")
	assert.Contains(t, out, "help[1]:", "the correction belongs inline, not behind a follow-up --help")
	assert.Contains(t, out, "--host")
}

func TestVLLMRejectsPositionalArgs(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"status", "unexpected"}, {"plan", "unexpected"}, {"apply", "unexpected"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, vllmCmd(setupTestApp(t)), args...)
			require.Error(t, err)
		})
	}
}

// The --confirm gate is checked before host or spec resolution, so it must
// refuse the same way with no tfvars gpu_vm_ip and no serving.yaml at all.
func TestVLLMApplyRefusesWithoutConfirmAndExitsTwo(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "apply")

	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err), "a missing --confirm is a usage problem, not a failure")
	assert.Contains(t, out, "confirm_required")
	assert.Contains(t, out, "talops vllm apply --confirm")
}

func TestVLLMApplyWithConfirmStillRefusesWithoutAGPUVMAddress(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "apply", "--confirm")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "vllm_host_unset")
}

func TestVLLMStatusRefusesWithoutAGPUVMAddress(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "status")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "vllm_host_unset")
	assert.Contains(t, out, "gpu_vm_ip")
}

func TestVLLMPlanRefusesWithoutAGPUVMAddress(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "plan")

	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, out, "vllm_host_unset")
}

func TestVLLMJSONRefusalIsOneObjectTaggedWithItsCommand(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "plan", "--json")

	require.Error(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], `"event":"plan"`)
	assert.Contains(t, lines[0], `"code":"vllm_host_unset"`)
}

// AXI's content-first rule: a bare invocation shows live state, because a
// caller can act on a drift report but must make a second call after help text.
func TestBareVLLMGroupShowsStateAndIdentifiesItself(t *testing.T) {
	out, _ := execute(t, vllmCmd(setupTestApp(t)))

	assert.Contains(t, out, "bin: ")
	assert.Contains(t, out, "description: ")
	assert.Contains(t, out, "vllm:", "the bare group reports status, not usage")
	assert.NotContains(t, out, "Usage:")
}

func TestVLLMHelpNamesEveryCommand(t *testing.T) {
	out, err := execute(t, vllmCmd(setupTestApp(t)), "--help")

	require.NoError(t, err)
	assert.Contains(t, out, "status")
	assert.Contains(t, out, "plan")
	assert.Contains(t, out, "apply")
}

func TestVLLMSubcommandHelpCarriesExamples(t *testing.T) {
	for _, sub := range []string{"status", "plan", "apply"} {
		t.Run(sub, func(t *testing.T) {
			out, err := execute(t, vllmCmd(setupTestApp(t)), sub, "--help")

			require.NoError(t, err)
			assert.Contains(t, out, "Examples:")
			assert.Contains(t, out, "talops vllm "+sub)
		})
	}
}

// Exit code 2 is claimed by two different contracts in this repo:
// confirm_required (usage) is the only one this group ever returns; every
// other refusal here is 1.
func TestVLLMOnlyConfirmRequiredExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--stat"},
		{"status"},
		{"plan"},
		{"apply", "--confirm"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := execute(t, vllmCmd(setupTestApp(t)), args...)
			require.Error(t, err)
			assert.NotEqual(t, 2, ExitCode(err))
			assert.Equal(t, 1, ExitCode(err))
		})
	}

	_, err := execute(t, vllmCmd(setupTestApp(t)), "apply")
	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
}
