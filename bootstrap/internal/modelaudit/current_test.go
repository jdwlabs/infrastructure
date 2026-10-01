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

func TestCurrentReadsMaxNumBatchedTokens(t *testing.T) {
	c, err := CurrentFromSpec(spec(pinnedImage,
		"--max-model-len=32768", "--tool-call-parser=hermes", "--max_num_batched_tokens", "8192"))
	require.NoError(t, err)
	assert.Equal(t, 8192, c.MaxNumBatchedTokens)

	_, err = CurrentFromSpec(spec(pinnedImage,
		"--max-model-len=32768", "--tool-call-parser=hermes", "--max-num-batched-tokens=0"))
	assert.ErrorContains(t, err, "--max-num-batched-tokens")
}

func TestCurrentRecordsServingFlagsTheHybridEstimateDoesNotModel(t *testing.T) {
	base := []string{"--max-model-len=32768", "--tool-call-parser=hermes"}
	cases := map[string]struct {
		args []string
		want []string
	}{
		"none":                           {nil, nil},
		"prefix caching on":              {[]string{"--enable-prefix-caching"}, []string{"--enable-prefix-caching"}},
		"prefix caching off is default":  {[]string{"--no-enable-prefix-caching"}, nil},
		"chunked prefill off":            {[]string{"--no-enable-chunked-prefill"}, []string{"--no-enable-chunked-prefill"}},
		"ssm dtype, two elements":        {[]string{"--mamba-ssm-cache-dtype", "float32"}, []string{"--mamba-ssm-cache-dtype"}},
		"ssm dtype, underscores":         {[]string{"--mamba_ssm_cache_dtype=float16"}, []string{"--mamba-ssm-cache-dtype"}},
		"ssm dtype auto is default":      {[]string{"--mamba-ssm-cache-dtype=auto"}, nil},
		"conv dtype":                     {[]string{"--mamba-cache-dtype=float32"}, []string{"--mamba-cache-dtype"}},
		"speculative config, = form":     {[]string{`--speculative-config={"method":"mtp","num_speculative_tokens":1}`}, []string{"--speculative-config"}},
		"speculative config, short form": {[]string{"-sc", `{"method":"mtp"}`}, []string{"--speculative-config"}},
		"several, in a fixed order": {
			[]string{"--speculative-config", "{}", "--enable-prefix-caching"},
			[]string{"--enable-prefix-caching", "--speculative-config"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cur, err := CurrentFromSpec(spec(pinnedImage, append(append([]string{}, base...), c.args...)...))
			require.NoError(t, err)
			assert.Equal(t, c.want, cur.UnmodelledFlags)
		})
	}
}

func TestCurrentReadsLanguageModelOnly(t *testing.T) {
	c, err := CurrentFromSpec(spec(pinnedImage, "--max-model-len=32768", "--tool-call-parser=hermes"))
	require.NoError(t, err)
	assert.False(t, c.LanguageModelOnly)

	c, err = CurrentFromSpec(spec(pinnedImage, "--max-model-len=32768", "--language_model_only", "--tool-call-parser=hermes"))
	require.NoError(t, err)
	assert.True(t, c.LanguageModelOnly)
}
