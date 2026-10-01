package modelaudit

import (
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c, err := ParseConfig([]byte(validConfig))
	require.NoError(t, err)
	c.Licenses.Allow = []string{"apache-2.0", "gemma"}
	c.Licenses.AllowNames = []string{"qwen-open"}
	return c
}

func item(mut func(*hub.ListItem)) hub.ListItem {
	it := hub.ListItem{
		ID:          "Qwen/Qwen3-Coder-Next-Instruct",
		SHA:         "1111111111111111111111111111111111111111",
		PipelineTag: "text-generation",
		CardData:    hub.CardData{License: "apache-2.0"},
	}
	if mut != nil {
		mut(&it)
	}
	return it
}

func TestPreFilter(t *testing.T) {
	cfg := testConfig(t)
	cases := map[string]struct {
		mut     func(*hub.ListItem)
		token   bool
		license string
		reason  string
	}{
		"passes":                     {nil, false, "apache-2.0", ""},
		"no sha":                     {func(i *hub.ListItem) { i.SHA = "" }, false, "", "no revision sha"},
		"pipeline from cardData":     {func(i *hub.ListItem) { i.PipelineTag = ""; i.CardData.PipelineTag = "image-text-to-text" }, false, "apache-2.0", ""},
		"pipeline missing":           {func(i *hub.ListItem) { i.PipelineTag = "" }, false, "", "pipeline_tag missing"},
		"pipeline not servable":      {func(i *hub.ListItem) { i.PipelineTag = "text-to-image" }, false, "", "pipeline_tag text-to-image"},
		"license from tag":           {func(i *hub.ListItem) { i.CardData.License = ""; i.Tags = []string{"license:gemma"} }, false, "gemma", ""},
		"license missing":            {func(i *hub.ListItem) { i.CardData.License = "" }, false, "", "license missing"},
		"license not allowed":        {func(i *hub.ListItem) { i.CardData.License = "cc-by-nc-4.0" }, false, "", "license cc-by-nc-4.0 not allowed"},
		"other with allowed name":    {func(i *hub.ListItem) { i.CardData.License = "other"; i.CardData.LicenseName = "qwen-open" }, false, "other:qwen-open", ""},
		"other with unlisted name":   {func(i *hub.ListItem) { i.CardData.License = "other"; i.CardData.LicenseName = "qwen-research" }, false, "", "license other (qwen-research) not allowed"},
		"other with no name":         {func(i *hub.ListItem) { i.CardData.License = "other" }, false, "", "license other (unnamed) not allowed"},
		"gated without token":        {func(i *hub.ListItem) { i.Gated = "manual" }, false, "", "gated (manual): licence acceptance and HF_TOKEN required"},
		"gated with token continues": {func(i *hub.ListItem) { i.Gated = "auto" }, true, "apache-2.0", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			license, reason := PreFilter(item(c.mut), cfg, c.token)
			assert.Equal(t, c.reason, reason)
			assert.Equal(t, c.license, license)
		})
	}
}

func TestInstructionTunedAcceptsAnyOneSignal(t *testing.T) {
	var bare hub.ModelInfo
	withTemplate := hub.ModelInfo{}
	withTemplate.Config.TokenizerConfig.ChatTemplate = []byte(`"{{ x }}"`)

	assert.True(t, InstructionTuned("Org/M-Instruct", bare, nil))
	assert.True(t, InstructionTuned("Org/M-Chat", bare, nil))
	assert.True(t, InstructionTuned("google/gemma-3-27b-it", bare, nil))
	assert.True(t, InstructionTuned("Org/M", hub.ModelInfo{Tags: []string{"conversational"}}, nil))
	assert.True(t, InstructionTuned("Org/M", withTemplate, nil))
	assert.True(t, InstructionTuned("Org/M", bare, []hub.TreeEntry{{Type: "file", Path: "chat_template.jinja"}}))

	assert.False(t, InstructionTuned("Org/M-Base", bare, nil))
	assert.False(t, InstructionTuned("Org/M-itinerary", bare, nil), "-it must end a word")
	assert.False(t, InstructionTuned("instruct-org/M-Base", bare, nil), "the org name is not the model name")
}

func TestInstructionTunedReadsTheRecordedIncumbent(t *testing.T) {
	var info hub.ModelInfo
	loadJSON(t, "incumbent-revision.json", &info)
	assert.True(t, info.HasChatTemplate())
	assert.True(t, InstructionTuned("QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", info, nil))
}

func TestResolveCandidateParser(t *testing.T) {
	cfg := testConfig(t)
	reg := Registry{"qwen3_xml": true, "hermes": true}

	r, reason := ResolveCandidateParser(cfg, reg, "v0.24.0", "Qwen/Qwen3-Coder-Next-Instruct", map[string]any{"model_type": "qwen3_moe"})
	assert.Empty(t, reason)
	assert.Equal(t, "qwen3_xml", r.Parser)

	r, reason = ResolveCandidateParser(cfg, reg, "v0.24.0", "Qwen/Qwen3-30B-A3B-Instruct-2507", map[string]any{"model_type": "qwen3_moe"})
	assert.Empty(t, reason)
	assert.Equal(t, "hermes", r.Parser)

	_, reason = ResolveCandidateParser(cfg, reg, "v0.24.0", "google/gemma-4-it", map[string]any{"model_type": "gemma4"})
	assert.Equal(t, "no parser rule for gemma4", reason)

	_, reason = ResolveCandidateParser(cfg, reg, "v0.24.0", "Org/M", map[string]any{})
	assert.Equal(t, "no parser rule for (none)", reason)

	_, reason = ResolveCandidateParser(cfg, reg, "v0.24.0", "mistralai/Mistral-Small-3.2-24B-Instruct-2506",
		map[string]any{"model_type": "mistral3", "text_config": map[string]any{"model_type": "mistral"}})
	assert.Equal(t, "parser mistral not in vLLM v0.24.0", reason)
}

func TestCheckArchitecture(t *testing.T) {
	archs := Architectures{"Qwen3_5ForConditionalGeneration": true, "LlamaForCausalLM": true}
	cases := map[string]struct {
		conf   map[string]any
		reason string
	}{
		"registered":   {map[string]any{"architectures": []any{"Qwen3_5ForConditionalGeneration"}}, ""},
		"unregistered": {map[string]any{"architectures": []any{"Qwen3_5ForCausalLM"}}, "architecture Qwen3_5ForCausalLM not in vLLM v0.24.0"},
		"a later entry is registered": {
			map[string]any{"architectures": []any{"CustomForCausalLM", "LlamaForCausalLM"}}, "",
		},
		"none registered names the first": {
			map[string]any{"architectures": []any{"AForCausalLM", "BForCausalLM"}}, "architecture AForCausalLM not in vLLM v0.24.0",
		},
		"missing":     {map[string]any{"model_type": "llama"}, "architecture missing"},
		"empty list":  {map[string]any{"architectures": []any{}}, "architecture missing"},
		"not strings": {map[string]any{"architectures": "LlamaForCausalLM"}, "architecture missing"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.reason, CheckArchitecture(archs, "v0.24.0", c.conf))
		})
	}
}
