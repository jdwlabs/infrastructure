package modelaudit

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile("audittest/testdata/" + name)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v))
}

func incumbent(t *testing.T) (map[string]any, []hub.TreeEntry) {
	var cfg map[string]any
	var tree []hub.TreeEntry
	loadJSON(t, "incumbent-config.json", &cfg)
	loadJSON(t, "incumbent-tree.json", &tree)
	return cfg, tree
}

// budget is serving.yaml's today: 32607 MiB at vLLM's default 0.90.
var budget = BudgetGiB(32607, 0.90)

func TestBudgetMatchesTheCard(t *testing.T) {
	assert.InDelta(t, 28.66, budget, 0.005)
}

// Calibration: the incumbent's real config.json and tree at c58857a7.
func TestIncumbentFitsWithTheCalibrationNumbers(t *testing.T) {
	cfg, tree := incumbent(t)

	fit := Estimate(cfg, tree, 32768, 3, budget)

	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 15.66, fit.WeightsGiB)
	assert.Equal(t, 3.00, fit.KVGiB)
	assert.Equal(t, 21.66, fit.TotalGiB)
	assert.Equal(t, 28.66, fit.BudgetGiB)
	assert.True(t, fit.Fits)
	assert.Empty(t, fit.Reason)
}

func TestMistralDualFormatCountsTheShardsOnce(t *testing.T) {
	var cfg map[string]any
	var tree []hub.TreeEntry
	loadJSON(t, "mistral-small-3.2-config.json", &cfg)
	loadJSON(t, "mistral-small-3.2-tree.json", &tree)

	fit := Estimate(cfg, tree, 32768, 3, budget)

	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 44.72, fit.WeightsGiB, "not 89.45: consolidated.safetensors duplicates the shards")
	assert.Equal(t, 5.00, fit.KVGiB, "read from text_config: 40 layers x 8 kv heads x 128")
	assert.False(t, fit.Fits)
	assert.Equal(t, "does not fit: 52.72 GiB > 28.66 GiB budget", fit.Reason)
}

func TestShardRuleWinsOverOtherRootFiles(t *testing.T) {
	tree := []hub.TreeEntry{
		{Type: "file", Path: "model-00001-of-00002.safetensors", Size: gib},
		{Type: "file", Path: "model-00002-of-00002.safetensors", Size: gib},
		{Type: "file", Path: "adapter.safetensors", Size: 7 * gib},
		{Type: "file", Path: "consolidated.safetensors", Size: 2 * gib},
	}
	w, reason := WeightsBytes(tree)
	assert.Empty(t, reason)
	assert.Equal(t, int64(2*gib), w)
}

func TestConsolidatedIsExcludedOnlyWhenAnotherRootFileExists(t *testing.T) {
	w, _ := WeightsBytes([]hub.TreeEntry{
		{Type: "file", Path: "model.safetensors", Size: 3 * gib},
		{Type: "file", Path: "consolidated.safetensors", Size: 3 * gib},
	})
	assert.Equal(t, int64(3*gib), w)

	w, _ = WeightsBytes([]hub.TreeEntry{{Type: "file", Path: "consolidated.safetensors", Size: 4 * gib}})
	assert.Equal(t, int64(4*gib), w)
}

func TestWeightsOnlyInSubfoldersAreFitUnknown(t *testing.T) {
	cfg, _ := incumbent(t)
	fit := Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "awq/model.safetensors", Size: gib}}, 32768, 3, budget)
	assert.False(t, fit.Known)
	assert.Equal(t, "fit unknown: no root safetensors weights", fit.Reason)

	fit = Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "model.gguf", Size: gib}}, 32768, 3, budget)
	assert.Equal(t, "fit unknown: no safetensors weights", fit.Reason)
}

func TestDense70BFP16DoesNotFit(t *testing.T) {
	cfg := map[string]any{
		"model_type": "llama", "num_hidden_layers": 80.0, "num_attention_heads": 64.0,
		"num_key_value_heads": 8.0, "hidden_size": 8192.0,
	}
	var tree []hub.TreeEntry
	for i := 1; i <= 30; i++ {
		tree = append(tree, hub.TreeEntry{Type: "file", Path: fmt.Sprintf("model-%05d-of-00030.safetensors", i), Size: 4_700_000_000})
	}
	fit := Estimate(cfg, tree, 32768, 3, budget)
	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 10.00, fit.KVGiB, "head_dim falls back to hidden_size / num_attention_heads = 128")
	assert.False(t, fit.Fits)
}

func TestQwen3NullSlidingWindowIsEstimated(t *testing.T) {
	cfg := map[string]any{
		"model_type": "qwen3", "num_hidden_layers": 36.0, "num_attention_heads": 32.0,
		"num_key_value_heads": 8.0, "head_dim": 128.0, "sliding_window": nil, "use_sliding_window": false,
	}
	fit := Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "model.safetensors", Size: 8 * gib}}, 32768, 3, budget)
	assert.True(t, fit.Known, fit.Reason)
	assert.True(t, fit.Fits)
}

func TestFitUnknownLayouts(t *testing.T) {
	cases := map[string]struct {
		cfg    map[string]any
		reason string
	}{
		"gpt-oss sliding_window 128 with no use_sliding_window": {
			map[string]any{"model_type": "gpt_oss", "sliding_window": 128.0, "num_hidden_layers": 24.0,
				"layer_types": []any{"sliding_attention", "full_attention"}},
			"fit unknown: sliding attention not modelled",
		},
		"MLA": {
			map[string]any{"model_type": "deepseek_v3", "kv_lora_rank": 512.0, "num_hidden_layers": 61.0},
			"fit unknown: MLA (kv_lora_rank) not modelled",
		},
		"granite hybrid layer_types": {
			map[string]any{"model_type": "granitemoehybrid", "num_hidden_layers": 40.0,
				"layer_types": []any{"mamba", "attention"}},
			"fit unknown: layer type mamba not modelled",
		},
		"gemma3 sliding window kept in text_config": {
			map[string]any{"model_type": "gemma3", "text_config": map[string]any{"sliding_window": 1024.0, "num_hidden_layers": 62.0}},
			"fit unknown: sliding attention not modelled",
		},
		"missing layers": {
			map[string]any{"model_type": "qwen3", "num_attention_heads": 32.0, "head_dim": 128.0},
			"fit unknown: config missing num_hidden_layers",
		},
		"missing heads": {
			map[string]any{"model_type": "qwen3", "num_hidden_layers": 36.0, "head_dim": 128.0},
			"fit unknown: config missing num_key_value_heads",
		},
		"missing head_dim and hidden_size": {
			map[string]any{"model_type": "qwen3", "num_hidden_layers": 36.0, "num_attention_heads": 32.0},
			"fit unknown: config missing head_dim",
		},
	}
	tree := []hub.TreeEntry{{Type: "file", Path: "model.safetensors", Size: gib}}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fit := Estimate(c.cfg, tree, 32768, 3, budget)
			assert.False(t, fit.Known)
			assert.False(t, fit.Fits)
			assert.Equal(t, c.reason, fit.Reason)
		})
	}
}
