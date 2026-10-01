package modelaudit

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordedRegistry(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile("audittest/testdata/tool_parsers_v0.24.0.py")
	require.NoError(t, err)
	return src
}

func TestParseRegistryReadsTheRecordedV0240Table(t *testing.T) {
	reg, err := ParseRegistry(recordedRegistry(t), "qwen3_xml")
	require.NoError(t, err)
	assert.Len(t, reg, 43)
	for _, p := range []string{"qwen3_xml", "qwen3_coder", "hermes", "llama3_json", "llama4_pythonic", "mistral", "glm45", "granite", "openai", "gemma4"} {
		assert.True(t, reg[p], p)
	}
	assert.False(t, reg["deepseekv3_tool_parser"], "a filename is a value, not a key")
}

func TestParseRegistryRejectsAMissingIncumbentParser(t *testing.T) {
	_, err := ParseRegistry(recordedRegistry(t), "qwen4_xml")
	assert.ErrorContains(t, err, `"qwen4_xml"`)
}

func TestParseRegistryRejectsBelowTheSanityFloor(t *testing.T) {
	src := "_TOOL_PARSERS_TO_REGISTER = {\n" +
		strings.Repeat("    \"only\": (\"f\", \"C\"),\n", 1) +
		"    \"qwen3_xml\": (\"f\", \"C\"),\n}\n"
	_, err := ParseRegistry([]byte(src), "qwen3_xml")
	assert.ErrorContains(t, err, "2 keys")
}

func TestParseRegistryRejectsAMissingTable(t *testing.T) {
	_, err := ParseRegistry([]byte("# moved\n"), "qwen3_xml")
	assert.ErrorContains(t, err, "not found")
}

func TestParseRegistryRejectsAnUnclosedTable(t *testing.T) {
	src := recordedRegistry(t)
	cut := src[:strings.Index(string(src), "\n}\n")]
	_, err := ParseRegistry(cut, "qwen3_xml")
	assert.ErrorContains(t, err, "not found")
}

// Recorded with
//
//	curl -sS -o audittest/testdata/model_registry_v0.24.0.py https://raw.githubusercontent.com/vllm-project/vllm/v0.24.0/vllm/model_executor/models/registry.py
func recordedModelRegistry(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile("audittest/testdata/model_registry_v0.24.0.py")
	require.NoError(t, err)
	return src
}

func TestParseModelRegistryReadsEveryTableVLLMModelsComposes(t *testing.T) {
	archs, err := ParseModelRegistry(recordedModelRegistry(t))
	require.NoError(t, err)
	assert.Len(t, archs, 359)
	for _, a := range []string{
		"LlamaForCausalLM", "Qwen3MoeForCausalLM", "Qwen3_5ForConditionalGeneration",
		"Qwen3_5MoeForConditionalGeneration", "Qwen3NextForCausalLM", "MiMoV2ForCausalLM",
		"Mistral3ForConditionalGeneration", "SmolLM3ForCausalLM", "TransformersForCausalLM",
	} {
		assert.True(t, archs[a], a)
	}
	assert.False(t, archs["Qwen3_5ForCausalLM"], "registered only as a module class, never as a key")
	assert.False(t, archs["qwen3_5"], "a module name is a value, not a key")
	assert.False(t, archs["MotifForCausalLM"], "_PREVIOUSLY_SUPPORTED_MODELS is not part of _VLLM_MODELS")
}

func TestParseModelRegistryRejectsAMissingComposition(t *testing.T) {
	_, err := ParseModelRegistry([]byte("_TEXT_GENERATION_MODELS = {\n    \"LlamaForCausalLM\": (\"llama\", \"LlamaForCausalLM\"),\n}\n"))
	assert.ErrorContains(t, err, "_VLLM_MODELS")
}

func TestParseModelRegistryRejectsAComposedTableItCannotFind(t *testing.T) {
	src := strings.Replace(string(recordedModelRegistry(t)), "_REWARD_MODELS = {", "_REWARD_MODELS = dict(", 1)
	_, err := ParseModelRegistry([]byte(src))
	assert.ErrorContains(t, err, "_REWARD_MODELS")
}

func TestParseModelRegistryRejectsAnUnexpectedCompositionLine(t *testing.T) {
	src := strings.Replace(string(recordedModelRegistry(t)), "    **_REWARD_MODELS,\n", "    **_REWARD_MODELS, **_EXTRA,\n", 1)
	_, err := ParseModelRegistry([]byte(src))
	assert.ErrorContains(t, err, "**_EXTRA")
}

func TestParseModelRegistryRejectsAnUnclosedTable(t *testing.T) {
	src := string(recordedModelRegistry(t))
	cut := src[:strings.Index(src, "_EMBEDDING_MODELS = {")]
	cut = cut[:strings.LastIndex(cut, "\n}\n")+1]
	_, err := ParseModelRegistry([]byte(cut + "\n_VLLM_MODELS = {\n    **_TEXT_GENERATION_MODELS,\n}\n"))
	assert.ErrorContains(t, err, "_TEXT_GENERATION_MODELS")
}

func TestParseModelRegistryRejectsBelowTheSanityFloor(t *testing.T) {
	src := "_TEXT_GENERATION_MODELS = {\n    \"LlamaForCausalLM\": (\"llama\", \"LlamaForCausalLM\"),\n}\n\n" +
		"_VLLM_MODELS = {\n    **_TEXT_GENERATION_MODELS,\n}\n"
	_, err := ParseModelRegistry([]byte(src))
	assert.ErrorContains(t, err, "1 architectures")
}

func TestParseModelRegistryRejectsATableWithoutLlama(t *testing.T) {
	src := strings.Replace(string(recordedModelRegistry(t)), "    \"LlamaForCausalLM\":", "    \"LlamaRenamedForCausalLM\":", 1)
	_, err := ParseModelRegistry([]byte(src))
	assert.ErrorContains(t, err, "LlamaForCausalLM")
}
