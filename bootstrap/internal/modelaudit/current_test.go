package modelaudit

import (
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/vllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func spec(image string, args ...string) vllm.Spec {
	return vllm.Spec{
		Image:      image,
		Model:      vllm.Model{Repo: "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", Revision: strings.Repeat("c", 40)},
		ServedName: "qwen/qwen3-coder-30b-a3b",
		Args:       args,
	}
}

const pinnedImage = "docker.io/vllm/vllm-openai:v0.24.0@sha256:251eba5cc7c12fed0b75da22a9240e582b1c9e39f6fbc064f86781b963bd814f"

func TestCurrentFromCommittedServingSpec(t *testing.T) {
	c, err := LoadCurrent("../../../inference/vllm/serving.yaml")
	require.NoError(t, err)
	assert.Equal(t, "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", c.Repo)
	assert.Equal(t, "v0.24.0", c.VLLMTag)
	assert.Equal(t, 32768, c.ContextTokens)
	assert.InDelta(t, 0.90, c.GPUMemoryUtilization, 1e-9)
	assert.Equal(t, "qwen3_xml", c.ToolCallParser)
	assert.Equal(t, "awq_marlin", c.Quantization)
}

func TestCurrentAcceptsBothFlagSpellings(t *testing.T) {
	c, err := CurrentFromSpec(spec(pinnedImage,
		"--max-model-len", "16384", "--gpu_memory_utilization=0.85", "--tool-call-parser", "hermes"))
	require.NoError(t, err)
	assert.Equal(t, 16384, c.ContextTokens)
	assert.InDelta(t, 0.85, c.GPUMemoryUtilization, 1e-9)
	assert.Equal(t, "hermes", c.ToolCallParser)
	assert.Equal(t, "", c.Quantization)
}

func TestCurrentRejects(t *testing.T) {
	cases := map[string]vllm.Spec{
		"tag not vX.Y.Z": spec("docker.io/vllm/vllm-openai:nightly@sha256:"+strings.Repeat("a", 64),
			"--max-model-len=32768", "--tool-call-parser=qwen3_xml"),
		"no max-model-len":      spec(pinnedImage, "--tool-call-parser=qwen3_xml"),
		"max-model-len not int": spec(pinnedImage, "--max-model-len=32k", "--tool-call-parser=qwen3_xml"),
		"utilisation above 1":   spec(pinnedImage, "--max-model-len=32768", "--gpu-memory-utilization=1.5", "--tool-call-parser=qwen3_xml"),
		"no tool-call-parser":   spec(pinnedImage, "--max-model-len=32768"),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := CurrentFromSpec(s)
			assert.Error(t, err)
		})
	}
}
