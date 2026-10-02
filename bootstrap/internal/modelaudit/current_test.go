package modelaudit

import (
	"strconv"
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

const (
	committedSpec = "../../../inference/vllm/serving.yaml"
	fixtureSpec   = "testdata/serving.yaml"
)

func TestCurrentFromServingSpecFile(t *testing.T) {
	c, err := LoadCurrent(fixtureSpec)
	require.NoError(t, err)
	assert.Equal(t, "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", c.Repo)
	assert.Equal(t, "c58857a7f41c0920f73d1b56678640f9c02017d7", c.Revision)
	assert.Equal(t, "local-chat", c.ServedName)
	assert.Equal(t, []string{"qwen/qwen3-coder-30b-a3b"}, c.ServedAliases)
	assert.Equal(t, "v0.24.0", c.VLLMTag)
	assert.Equal(t, 32768, c.ContextTokens)
	assert.InDelta(t, 0.90, c.GPUMemoryUtilization, 1e-9)
	assert.Equal(t, "qwen3_xml", c.ToolCallParser)
	assert.Equal(t, "awq_marlin", c.Quantization)
	assert.False(t, c.LanguageModelOnly)
}

// A validity guard, not a content freeze: the served model changes by PR and
// reverts by PR, so this asserts only that the audit can read whatever
// serving.yaml holds and that what it reads agrees with the file's own
// fields and flags.
func TestCommittedServingSpecYieldsACurrent(t *testing.T) {
	s, err := vllm.Load(committedSpec)
	require.NoError(t, err)
	c, err := CurrentFromSpec(s)
	require.NoError(t, err)

	assert.Equal(t, s.Model.Repo, c.Repo)
	assert.Equal(t, s.Model.Revision, c.Revision)
	assert.Equal(t, s.ServedName, c.ServedName)
	assert.Equal(t, s.ServedAliases, c.ServedAliases)
	assert.Contains(t, s.Image, ":"+c.VLLMTag+"@")

	flags := map[string]string{}
	for i, a := range s.Args {
		name, val, hasEq := strings.Cut(a, "=")
		if !hasEq && i+1 < len(s.Args) && !strings.HasPrefix(s.Args[i+1], "--") {
			val = s.Args[i+1]
		}
		flags[strings.ReplaceAll(name, "_", "-")] = val
	}
	assert.Equal(t, flags["--max-model-len"], strconv.Itoa(c.ContextTokens))
	assert.Equal(t, flags["--tool-call-parser"], c.ToolCallParser)
	assert.Equal(t, flags["--quantization"], c.Quantization)
	_, lmOnly := flags["--language-model-only"]
	assert.Equal(t, lmOnly, c.LanguageModelOnly)
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

func TestCurrentCarriesServedAliases(t *testing.T) {
	s := spec(pinnedImage, "--max-model-len=32768", "--tool-call-parser=qwen3_xml")
	s.ServedName = "local-chat"
	s.ServedAliases = []string{"qwen/qwen3-coder-30b-a3b"}

	c, err := CurrentFromSpec(s)
	require.NoError(t, err)
	assert.Equal(t, "local-chat", c.ServedName)
	assert.Equal(t, []string{"qwen/qwen3-coder-30b-a3b"}, c.ServedAliases)
}
