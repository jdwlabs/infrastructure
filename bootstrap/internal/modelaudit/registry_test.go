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
