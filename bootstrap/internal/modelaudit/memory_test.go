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

// batched is vLLM's default max_num_batched_tokens for an OpenAI API server
// on a card under 70 GiB, which this one is.
var batched = BatchedTokens(Current{}, 32607)

func TestBudgetMatchesTheCard(t *testing.T) {
	assert.InDelta(t, 28.66, budget, 0.005)
}

func TestBatchedTokensFollowsVLLMsDefaultForTheCard(t *testing.T) {
	assert.Equal(t, 2048, BatchedTokens(Current{}, 32607))
	assert.Equal(t, 8192, BatchedTokens(Current{}, 81559), "an 80 GB card gets the larger default")
	assert.Equal(t, 4096, BatchedTokens(Current{MaxNumBatchedTokens: 4096}, 32607), "the serving flag wins")
}

// Calibration: the incumbent's real config.json and tree at c58857a7.
func TestIncumbentFitsWithTheCalibrationNumbers(t *testing.T) {
	cfg, tree := incumbent(t)

	fit := Estimate(cfg, tree, 32768, batched, 3, budget)

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

	fit := Estimate(cfg, tree, 32768, batched, 3, budget)

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
	fit := Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "awq/model.safetensors", Size: gib}}, 32768, batched, 3, budget)
	assert.False(t, fit.Known)
	assert.Equal(t, "fit unknown: no root safetensors weights", fit.Reason)

	fit = Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "model.gguf", Size: gib}}, 32768, batched, 3, budget)
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
	fit := Estimate(cfg, tree, 32768, batched, 3, budget)
	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 10.00, fit.KVGiB, "head_dim falls back to hidden_size / num_attention_heads = 128")
	assert.False(t, fit.Fits)
}

func TestQwen3NullSlidingWindowIsEstimated(t *testing.T) {
	cfg := map[string]any{
		"model_type": "qwen3", "num_hidden_layers": 36.0, "num_attention_heads": 32.0,
		"num_key_value_heads": 8.0, "head_dim": 128.0, "sliding_window": nil, "use_sliding_window": false,
	}
	fit := Estimate(cfg, []hub.TreeEntry{{Type: "file", Path: "model.safetensors", Size: 8 * gib}}, 32768, batched, 3, budget)
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
			fit := Estimate(c.cfg, tree, 32768, batched, 3, budget)
			assert.False(t, fit.Known)
			assert.False(t, fit.Fits)
			assert.Equal(t, c.reason, fit.Reason)
		})
	}
}

func fixture(t *testing.T, config, tree string) (map[string]any, []hub.TreeEntry) {
	t.Helper()
	var cfg map[string]any
	var entries []hub.TreeEntry
	loadJSON(t, config, &cfg)
	loadJSON(t, tree, &entries)
	return cfg, entries
}

// Calibration: RedHatAI/Qwen3.8-27B-NVFP4 at 2889ddeeda282f17fc9e159a42125e535159a5e5,
// recorded with
//
//	curl -sSL https://huggingface.co/RedHatAI/Qwen3.8-27B-NVFP4/resolve/<sha>/config.json | jq -c '{architectures, model_type, quantization_config: {quant_method: .quantization_config.quant_method}, text_config: (.text_config | {model_type, dtype, num_hidden_layers, num_attention_heads, num_key_value_heads, head_dim, hidden_size, layer_types, linear_num_key_heads, linear_num_value_heads, linear_key_head_dim, linear_value_head_dim, linear_conv_kernel_dim, mamba_ssm_dtype})}'
//	curl -sSL 'https://huggingface.co/api/models/RedHatAI/Qwen3.8-27B-NVFP4/tree/<sha>?recursive=true' | jq -c '.[] | {type, path, size}'
//
// By hand:
//
//	weights: the two model-0000N-of-00002 shards, 23839095472 B = 22.20 GiB
//	         (model_mtp.safetensors is the unused speculative head)
//	16 full_attention layers: 16 x 2 x 4 kv heads x 256 x 2 B x 32768 = 2147483648 B = 2.00 GiB
//	48 linear_attention layers, one state each:
//	  conv: (128 x 16 x 2 + 128 x 48) x (4 - 1) x 2 B         =   61440 B
//	  recurrent: 48 x 128 x 128 x 4 B (mamba_ssm_dtype float32) = 3145728 B
//	  48 x 3207168 B = 153944064 B = 0.14 GiB
//	kv + state: 2301427712 B = 2.14 GiB
//	total: 22.20 + 2.14 + 3 = 27.35 GiB, under the 28.66 GiB budget
func TestQwen35HybridIsPricedPerLayerType(t *testing.T) {
	cfg, tree := fixture(t, "qwen3.8-27b-nvfp4-config.json", "qwen3.8-27b-nvfp4-tree.json")

	fit := Estimate(cfg, tree, 32768, batched, 3, budget)

	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 22.20, fit.WeightsGiB)
	assert.Equal(t, 2.14, fit.KVGiB)
	assert.Equal(t, 27.35, fit.TotalGiB)
	assert.True(t, fit.Fits)
}

// Calibration: XiaomiMiMo/MiMo-V2.6-Flash-RL at 5711b268169967567844e1e560e8a3966da959b1,
// recorded with
//
//	curl -sSL https://huggingface.co/XiaomiMiMo/MiMo-V2.6-Flash-RL/resolve/<sha>/config.json | jq -c '{architectures, model_type, dtype, quantization_config: {quant_method: .quantization_config.quant_method}, num_hidden_layers, num_attention_heads, num_key_value_heads, head_dim, v_head_dim, hidden_size, hybrid_layer_pattern, sliding_window, sliding_window_size, swa_num_attention_heads, swa_num_key_value_heads, swa_head_dim, swa_v_head_dim}'
//	curl -sSL 'https://huggingface.co/api/models/XiaomiMiMo/MiMo-V2.6-Flash-RL/tree/<sha>?recursive=true' | jq -c '.[] | {type, path, size}'
//
// By hand, with hybrid_layer_pattern holding 9 zeros (full) and 39 ones (sliding):
//
//	weights: 65 root safetensors, 172932505264 B = 161.06 GiB
//	9 full layers: 9 x 4 kv heads x (192 k + 128 v) x 2 B x 32768 = 754974720 B
//	39 sliding layers hold min(128 - 1 + 2048 batched, 32768) = 2175 tokens:
//	  39 x 8 kv heads x (192 k + 128 v) x 2 B x 2175 = 434304000 B
//	kv: 1189278720 B = 1.11 GiB
//	total: 161.06 + 1.11 + 3 = 165.16 GiB
func TestMiMoV2SlidingLayersArePricedAtTheirWindow(t *testing.T) {
	cfg, tree := fixture(t, "mimo-v2.6-flash-rl-config.json", "mimo-v2.6-flash-rl-tree.json")

	fit := Estimate(cfg, tree, 32768, batched, 3, budget)

	require.True(t, fit.Known, fit.Reason)
	assert.Equal(t, 161.06, fit.WeightsGiB)
	assert.Equal(t, 1.11, fit.KVGiB)
	assert.Equal(t, "does not fit: 165.16 GiB > 28.66 GiB budget", fit.Reason)
}

func gdnConfig(modelType, arch string) map[string]any {
	return map[string]any{
		"architectures": []any{arch}, "model_type": modelType,
		"text_config": map[string]any{
			"num_hidden_layers": 4.0, "num_attention_heads": 8.0, "num_key_value_heads": 2.0, "head_dim": 128.0,
			"layer_types":          []any{"linear_attention", "linear_attention", "linear_attention", "full_attention"},
			"linear_num_key_heads": 4.0, "linear_num_value_heads": 8.0, "linear_key_head_dim": 64.0,
			"linear_value_head_dim": 64.0, "linear_conv_kernel_dim": 4.0, "mamba_ssm_dtype": "float32",
		},
	}
}

// One full layer is 2 x 2 x 128 x 2 B x 1024 = 1 MiB. Each linear layer's
// conv state is (64 x 4 x 2 + 64 x 8) x 3 x 2 B = 6 KiB and its recurrent
// state 8 x 64 x 64 elements = 32768, so three layers differ by 3 x 64 KiB
// between float32 and bfloat16.
func TestGatedDeltaNetRecurrentStateDtype(t *testing.T) {
	const mib = 1 << 20
	kv := func(cfg map[string]any) float64 {
		b, reason := CacheBytes(cfg, 1024, batched)
		require.Empty(t, reason)
		return b
	}
	assert.InDelta(t, mib+3*(6*1024+32768*4), kv(gdnConfig("qwen3_5", "Qwen3_5ForConditionalGeneration")), 0,
		"the config's mamba_ssm_dtype is applied to Qwen3.5 multimodal wrappers")
	assert.InDelta(t, mib+3*(6*1024+32768*4), kv(gdnConfig("qwen3_5_moe", "Qwen3_5MoeForConditionalGeneration")), 0)
	assert.InDelta(t, mib+3*(6*1024+32768*2), kv(gdnConfig("qwen3_next", "Qwen3NextForCausalLM")), 0,
		"Qwen3-Next ignores mamba_ssm_dtype and keeps the model dtype")
}

func TestHybridLayoutsThatCannotBePricedAreFitUnknown(t *testing.T) {
	mutate := func(base map[string]any, f func(cfg, text map[string]any)) map[string]any {
		raw, _ := json.Marshal(base)
		var cfg map[string]any
		_ = json.Unmarshal(raw, &cfg)
		text, _ := cfg["text_config"].(map[string]any)
		f(cfg, text)
		return cfg
	}
	gdn := gdnConfig("qwen3_5", "Qwen3_5ForConditionalGeneration")
	var mimo map[string]any
	loadJSON(t, "mimo-v2.6-flash-rl-config.json", &mimo)

	cases := map[string]struct {
		cfg    map[string]any
		reason string
	}{
		"linear_attention outside the gated delta net families": {
			mutate(gdn, func(cfg, _ map[string]any) { cfg["model_type"] = "kimi_linear" }),
			"fit unknown: layer type linear_attention not modelled",
		},
		"gated delta net field missing": {
			mutate(gdn, func(_, text map[string]any) { delete(text, "linear_num_value_heads") }),
			"fit unknown: config missing linear_num_value_heads",
		},
		"gated delta net without explicit layer_types": {
			mutate(gdn, func(_, text map[string]any) { delete(text, "layer_types") }),
			"fit unknown: config missing layer_types",
		},
		"layer_types shorter than the layer count": {
			mutate(gdn, func(_, text map[string]any) { text["num_hidden_layers"] = 5.0 }),
			"fit unknown: layer_types has 4 entries for 5 layers",
		},
		"unrecognised recurrent state dtype": {
			mutate(gdn, func(_, text map[string]any) { text["mamba_ssm_dtype"] = "float8" }),
			"fit unknown: mamba_ssm_dtype float8 not modelled",
		},
		"MiMo pattern shorter than the layer count": {
			mutate(mimo, func(cfg, _ map[string]any) { cfg["num_hidden_layers"] = 49.0 }),
			"fit unknown: hybrid_layer_pattern has 48 entries for 49 layers",
		},
		"MiMo pattern entry neither 0 nor 1": {
			mutate(mimo, func(cfg, _ map[string]any) { cfg["hybrid_layer_pattern"].([]any)[3] = 2.0 }),
			"fit unknown: hybrid_layer_pattern entry 2 not modelled",
		},
		"MiMo without a pattern": {
			mutate(mimo, func(cfg, _ map[string]any) { delete(cfg, "hybrid_layer_pattern") }),
			"fit unknown: config missing hybrid_layer_pattern",
		},
		"MiMo sliding layers without a window": {
			mutate(mimo, func(cfg, _ map[string]any) { delete(cfg, "sliding_window_size") }),
			"fit unknown: config missing sliding_window_size",
		},
		"MiMo sliding layers without their kv heads": {
			mutate(mimo, func(cfg, _ map[string]any) { delete(cfg, "swa_num_key_value_heads") }),
			"fit unknown: config missing swa_num_key_value_heads",
		},
		"MLA still wins over a sliding pattern": {
			mutate(mimo, func(cfg, _ map[string]any) { cfg["kv_lora_rank"] = 512.0 }),
			"fit unknown: MLA (kv_lora_rank) not modelled",
		},
	}
	tree := []hub.TreeEntry{{Type: "file", Path: "model.safetensors", Size: gib}}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fit := Estimate(c.cfg, tree, 32768, batched, 3, budget)
			assert.False(t, fit.Known)
			assert.False(t, fit.Fits)
			assert.Equal(t, c.reason, fit.Reason)
		})
	}
}
