package modelaudit

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
)

const gib = 1 << 30

// Fit is the memory estimate weights + kv + overhead against the budget.
// Known is false when the estimate cannot be trusted; that is a rejection,
// never an assumed fit.
type Fit struct {
	Known       bool
	Reason      string
	WeightsGiB  float64
	KVGiB       float64
	OverheadGiB float64
	TotalGiB    float64
	BudgetGiB   float64
	// MarginGiB is BudgetGiB - TotalGiB: how much the constant overheadGiB
	// can be wrong by before the model stops fitting.
	MarginGiB float64
	Fits      bool
}

// BudgetGiB is the memory vLLM may claim: the card's total times
// --gpu-memory-utilization.
func BudgetGiB(gpuMemMiB int, utilization float64) float64 {
	return float64(gpuMemMiB) / 1024 * utilization
}

var shardRe = regexp.MustCompile(`^model-\d{5}-of-\d{5}\.safetensors$`)

// WeightsBytes sums the weight files vLLM would load. Official Mistral repos
// ship the native consolidated file beside the HF shards, so summing every
// file counts the model twice; the HF shard set wins when present.
func WeightsBytes(tree []hub.TreeEntry) (int64, string) {
	var shards, plain, consolidated int64
	var nShards, nPlain, nConsolidated, nNested int
	for _, e := range tree {
		if e.Type != "file" || !strings.HasSuffix(e.Path, ".safetensors") {
			continue
		}
		if strings.Contains(e.Path, "/") {
			nNested++
			continue
		}
		switch {
		case shardRe.MatchString(e.Path):
			shards += e.Size
			nShards++
		case strings.HasPrefix(e.Path, "consolidated"):
			consolidated += e.Size
			nConsolidated++
		default:
			plain += e.Size
			nPlain++
		}
	}
	switch {
	case nShards > 0:
		return shards, ""
	case nPlain > 0:
		return plain, ""
	case nConsolidated > 0:
		return consolidated, ""
	case nNested > 0:
		return 0, "fit unknown: no root safetensors weights"
	default:
		return 0, "fit unknown: no safetensors weights"
	}
}

// field reads a config value from the top level, else from text_config,
// where multimodal wrappers (Mistral3, Gemma3) keep their language model's.
func field(cfg map[string]any, name string) any {
	if v, ok := cfg[name]; ok && v != nil {
		return v
	}
	if tc, ok := cfg["text_config"].(map[string]any); ok {
		if v, ok := tc[name]; ok && v != nil {
			return v
		}
	}
	return nil
}

func number(cfg map[string]any, name string) (float64, bool) {
	f, ok := field(cfg, name).(float64)
	return f, ok && f > 0
}

// elemBytes is the KV cache and conv state element size. vLLM's
// --kv-cache-dtype and --mamba-cache-dtype both default to the model dtype,
// which is 2 bytes for every model this audit can serve.
const elemBytes = 2

// BatchedTokens is the max_num_batched_tokens vLLM will run with: the
// serving flag, else v0.24.0's default for an OpenAI API server, which is
// 8192 on a card of at least 70 GiB and 2048 below it
// (vllm/engine/arg_utils.py:2404-2423). An 80 GB A100 also gets 2048 there;
// it is not told apart here because the larger figure only over-prices
// sliding layers.
func BatchedTokens(cur Current, gpuMemMiB int) int {
	if cur.MaxNumBatchedTokens > 0 {
		return cur.MaxNumBatchedTokens
	}
	if gpuMemMiB >= 70*1024 {
		return 8192
	}
	return 2048
}

// Serving is what serving.yaml decides about the cache besides the context
// length.
type Serving struct {
	BatchedTokens int
	// UnmodelledFlags are flags set away from the defaults the linear and
	// sliding formulas assume; see unmodelledFlags.
	UnmodelledFlags []string
}

func ServingFor(cur Current, gpuMemMiB int) Serving {
	return Serving{BatchedTokens: BatchedTokens(cur, gpuMemMiB), UnmodelledFlags: cur.UnmodelledFlags}
}

// flagReason rejects a linear or sliding layout priced under a serving flag
// it assumes is at its default. Full attention is unaffected by these flags.
func (sv Serving) flagReason() string {
	if len(sv.UnmodelledFlags) == 0 {
		return ""
	}
	return "fit unknown: serving flag " + sv.UnmodelledFlags[0] + " not modelled"
}

func missing(name string) string { return "fit unknown: config missing " + name }

// requireNumbers reads positive numeric fields, naming the first one absent.
func requireNumbers(cfg map[string]any, names ...string) ([]float64, string) {
	out := make([]float64, len(names))
	for i, n := range names {
		v, ok := number(cfg, n)
		if !ok {
			return nil, missing(n)
		}
		out[i] = v
	}
	return out, ""
}

func architectures(cfg map[string]any) []string {
	var out []string
	list, _ := cfg["architectures"].([]any)
	for _, a := range list {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// gatedDeltaNetTypes are the model types whose vLLM v0.24.0 classes size a
// linear_attention layer with gated_delta_net_state_shape (qwen3_5.py:699-718,
// qwen3_next.py:849-868). Another family's linear_attention is a different
// kernel with a different state, so it stays fit unknown.
var gatedDeltaNetTypes = map[string]bool{"qwen3_5": true, "qwen3_5_moe": true, "qwen3_next": true}

// CacheBytes is what vLLM v0.24.0 reserves for one sequence of contextTokens:
// each layer's max_memory_usage_bytes (vllm/v1/kv_cache_interface.py). Its
// own startup check prices grouped, padded pages instead
// (v1/core/kv_cache_utils.py); the block rounding and group padding that
// adds are left out, 0.01 GiB for Qwen3.8. A layout it cannot price is a
// reason, never a guess.
func CacheBytes(cfg map[string]any, contextTokens int, sv Serving) (float64, string) {
	if field(cfg, "kv_lora_rank") != nil {
		return 0, "fit unknown: MLA (kv_lora_rank) not modelled"
	}
	top, _ := ModelTypes(cfg)
	if top == "mimo_v2" {
		return mimoV2Bytes(cfg, contextTokens, sv)
	}
	// Sliding attention counts only when it actually runs: Qwen3 configs
	// carry sliding_window: null beside use_sliding_window: false, and
	// counting the key's presence would reject every Qwen3 model.
	if field(cfg, "sliding_window") != nil {
		if use, ok := field(cfg, "use_sliding_window").(bool); !ok || use {
			return 0, "fit unknown: sliding attention not modelled"
		}
	}
	types, hasTypes := field(cfg, "layer_types").([]any)
	var nLinear int
	for _, t := range types {
		switch {
		case t == "full_attention":
		case t == "linear_attention" && gatedDeltaNetTypes[top]:
			nLinear++
		default:
			return 0, fmt.Sprintf("fit unknown: layer type %v not modelled", t)
		}
	}
	// vLLM fills a default layer_types for these families; pricing that
	// default would be a guess about what the checkpoint holds.
	if !hasTypes && gatedDeltaNetTypes[top] {
		return 0, missing("layer_types")
	}

	layersF, ok := number(cfg, "num_hidden_layers")
	if !ok {
		return 0, missing("num_hidden_layers")
	}
	layers := int(layersF)
	if hasTypes && len(types) != layers {
		return 0, fmt.Sprintf("fit unknown: layer_types has %d entries for %d layers", len(types), layers)
	}
	if nLinear > 0 {
		if r := sv.flagReason(); r != "" {
			return 0, r
		}
	}
	perToken, reason := fullAttentionBytesPerToken(cfg)
	if reason != "" {
		return 0, reason
	}
	total := float64(layers-nLinear) * perToken * float64(contextTokens)
	if nLinear > 0 {
		state, reason := gatedDeltaNetStateBytes(cfg)
		if reason != "" {
			return 0, reason
		}
		total += float64(nLinear) * state
	}
	return total, ""
}

// fullAttentionBytesPerToken is K and V at one token for one layer.
func fullAttentionBytesPerToken(cfg map[string]any) (float64, string) {
	heads, hasHeads := number(cfg, "num_attention_heads")
	kvHeads, ok := number(cfg, "num_key_value_heads")
	if !ok {
		if !hasHeads {
			return 0, missing("num_key_value_heads")
		}
		kvHeads = heads
	}
	headDim, ok := number(cfg, "head_dim")
	if !ok {
		hidden, hasHidden := number(cfg, "hidden_size")
		if !hasHidden || !hasHeads {
			return 0, missing("head_dim")
		}
		headDim = hidden / heads
	}
	return 2 * kvHeads * headDim * elemBytes, ""
}

// gatedDeltaNetStateBytes is one linear_attention layer's state, fixed in
// size whatever the context: a conv state of conv_dim x (kernel - 1) and a
// recurrent state of value_heads x value_dim x key_dim
// (vllm/model_executor/layers/mamba/mamba_utils.py:213-234). One state per
// layer is reserved because hybrid models default to prefix caching off
// (vllm/config/model.py:1832-1837, engine/arg_utils.py:2492-2493), which
// leaves mamba_cache_mode "none" (model_executor/models/config.py:454-460,
// v1/kv_cache_interface.py:657). The serving flags that would change either
// assumption are rejected before this runs (Serving.flagReason).
func gatedDeltaNetStateBytes(cfg map[string]any) (float64, string) {
	v, reason := requireNumbers(cfg, "linear_num_key_heads", "linear_num_value_heads",
		"linear_key_head_dim", "linear_value_head_dim", "linear_conv_kernel_dim")
	if reason != "" {
		return 0, reason
	}
	kHeads, vHeads, kDim, vDim, kernel := v[0], v[1], v[2], v[3], v[4]
	ssmBytes, reason := recurrentStateBytes(cfg)
	if reason != "" {
		return 0, reason
	}
	conv := (kDim*kHeads*2 + vDim*vHeads) * (kernel - 1) * elemBytes
	recurrent := vHeads * vDim * kDim * ssmBytes
	return conv + recurrent, ""
}

// recurrentStateBytes follows _mamba_state_dtype (mamba_utils.py:84-96): the
// recurrent state takes mamba_ssm_cache_dtype, which defaults to the conv
// dtype. Only the two Qwen3.5 multimodal wrappers set it from the config's
// mamba_ssm_dtype (model_executor/models/config.py:603-628, 704-705), so
// Qwen3-Next and a text-only Qwen3.5 keep 2 bytes. Any listed architecture
// counts, because vLLM tries each in turn and float32 is the larger guess.
func recurrentStateBytes(cfg map[string]any) (float64, string) {
	wrapped := false
	for _, a := range architectures(cfg) {
		if a == "Qwen3_5ForConditionalGeneration" || a == "Qwen3_5MoeForConditionalGeneration" {
			wrapped = true
		}
	}
	dtype := field(cfg, "mamba_ssm_dtype")
	if !wrapped || dtype == nil {
		return elemBytes, ""
	}
	switch dtype {
	case "float32":
		return 4, ""
	case "float16", "bfloat16":
		return 2, ""
	default:
		return 0, fmt.Sprintf("fit unknown: mamba_ssm_dtype %v not modelled", dtype)
	}
}

// mimoV2Bytes prices MiMo-V2 the way vllm/model_executor/models/mimo_v2.py
// builds it: hybrid_layer_pattern[i] == 1 is a sliding layer with the swa_*
// head counts and sliding_window_size, 0 a full layer with the plain ones
// (mimo_v2.py:371-400, 458-459). K and V head dims differ, so a token costs
// kv_heads x (k_dim + v_dim) (kv_cache_interface.py:497-503). A sliding layer
// holds window - 1 + max_num_batched_tokens tokens, not the window, because
// a chunked-prefill step keeps the previous window beside the new chunk
// (kv_cache_interface.py:506-526).
func mimoV2Bytes(cfg map[string]any, contextTokens int, sv Serving) (float64, string) {
	pattern, ok := field(cfg, "hybrid_layer_pattern").([]any)
	if !ok {
		return 0, missing("hybrid_layer_pattern")
	}
	layersF, ok := number(cfg, "num_hidden_layers")
	if !ok {
		return 0, missing("num_hidden_layers")
	}
	if len(pattern) != int(layersF) {
		return 0, fmt.Sprintf("fit unknown: hybrid_layer_pattern has %d entries for %d layers", len(pattern), int(layersF))
	}
	var nFull, nSliding float64
	for _, p := range pattern {
		switch p {
		case 0.0:
			nFull++
		case 1.0:
			nSliding++
		default:
			return 0, fmt.Sprintf("fit unknown: hybrid_layer_pattern entry %v not modelled", p)
		}
	}
	if nSliding > 0 {
		if r := sv.flagReason(); r != "" {
			return 0, r
		}
	}
	var total float64
	if nFull > 0 {
		perToken, reason := mimoBytesPerToken(cfg, "num_key_value_heads", "head_dim", "v_head_dim")
		if reason != "" {
			return 0, reason
		}
		total += nFull * perToken * float64(contextTokens)
	}
	if nSliding > 0 {
		perToken, reason := mimoBytesPerToken(cfg, "swa_num_key_value_heads", "swa_head_dim", "swa_v_head_dim")
		if reason != "" {
			return 0, reason
		}
		window, ok := number(cfg, "sliding_window_size")
		if !ok {
			return 0, missing("sliding_window_size")
		}
		held := min(window-1+float64(sv.BatchedTokens), float64(contextTokens))
		total += nSliding * perToken * held
	}
	return total, ""
}

// mimoBytesPerToken reads a V head dim that falls back to the K one, as
// MiMoV2Attention does when the config omits it.
func mimoBytesPerToken(cfg map[string]any, kvHeadsKey, kDimKey, vDimKey string) (float64, string) {
	v, reason := requireNumbers(cfg, kvHeadsKey, kDimKey)
	if reason != "" {
		return 0, reason
	}
	vDim, ok := number(cfg, vDimKey)
	if !ok {
		vDim = v[1]
	}
	return v[0] * (v[1] + vDim) * elemBytes, ""
}

// Estimate is weights + CacheBytes + overhead against the budget.
func Estimate(cfg map[string]any, tree []hub.TreeEntry, contextTokens int, sv Serving, overheadGiB, budgetGiB float64) Fit {
	fit := Fit{OverheadGiB: overheadGiB, BudgetGiB: round2(budgetGiB)}
	kvBytes, reason := CacheBytes(cfg, contextTokens, sv)
	if reason != "" {
		fit.Reason = reason
		return fit
	}
	weights, reason := WeightsBytes(tree)
	if reason != "" {
		fit.Reason = reason
		return fit
	}

	fit.Known = true
	fit.WeightsGiB = round2(float64(weights) / gib)
	fit.KVGiB = round2(kvBytes / gib)
	total := float64(weights)/gib + kvBytes/gib + overheadGiB
	fit.TotalGiB = round2(total)
	fit.MarginGiB = round2(budgetGiB - total)
	fit.Fits = total <= budgetGiB
	if !fit.Fits {
		fit.Reason = fmt.Sprintf("does not fit: %.2f GiB > %.2f GiB budget", total, budgetGiB)
	}
	return fit
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
