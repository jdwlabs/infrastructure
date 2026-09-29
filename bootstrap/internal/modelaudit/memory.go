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
	Fits        bool
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

// layoutNotModelled names attention the formula cannot price. Sliding
// attention counts only when it actually runs: Qwen3 configs carry
// sliding_window: null beside use_sliding_window: false, and counting the
// key's presence would reject every Qwen3 model.
func layoutNotModelled(cfg map[string]any) string {
	if field(cfg, "sliding_window") != nil {
		if use, ok := field(cfg, "use_sliding_window").(bool); !ok || use {
			return "fit unknown: sliding attention not modelled"
		}
	}
	if types, ok := field(cfg, "layer_types").([]any); ok {
		for _, t := range types {
			if t != "full_attention" {
				return fmt.Sprintf("fit unknown: layer type %v not modelled", t)
			}
		}
	}
	return ""
}

// Estimate models the KV cache at 2 bytes per element for every layer, which
// is only true of full attention without MLA; anything else is fit unknown.
func Estimate(cfg map[string]any, tree []hub.TreeEntry, contextTokens int, overheadGiB, budgetGiB float64) Fit {
	fit := Fit{OverheadGiB: overheadGiB, BudgetGiB: round2(budgetGiB)}
	if field(cfg, "kv_lora_rank") != nil {
		fit.Reason = "fit unknown: MLA (kv_lora_rank) not modelled"
		return fit
	}
	if r := layoutNotModelled(cfg); r != "" {
		fit.Reason = r
		return fit
	}
	layers, ok := number(cfg, "num_hidden_layers")
	if !ok {
		fit.Reason = "fit unknown: config missing num_hidden_layers"
		return fit
	}
	heads, hasHeads := number(cfg, "num_attention_heads")
	kvHeads, ok := number(cfg, "num_key_value_heads")
	if !ok {
		if !hasHeads {
			fit.Reason = "fit unknown: config missing num_key_value_heads"
			return fit
		}
		kvHeads = heads
	}
	headDim, ok := number(cfg, "head_dim")
	if !ok {
		hidden, hasHidden := number(cfg, "hidden_size")
		if !hasHidden || !hasHeads {
			fit.Reason = "fit unknown: config missing head_dim"
			return fit
		}
		headDim = hidden / heads
	}
	weights, reason := WeightsBytes(tree)
	if reason != "" {
		fit.Reason = reason
		return fit
	}

	kvBytes := 2 * layers * kvHeads * headDim * 2 * float64(contextTokens)
	fit.Known = true
	fit.WeightsGiB = round2(float64(weights) / gib)
	fit.KVGiB = round2(kvBytes / gib)
	total := float64(weights)/gib + kvBytes/gib + overheadGiB
	fit.TotalGiB = round2(total)
	fit.Fits = total <= budgetGiB
	if !fit.Fits {
		fit.Reason = fmt.Sprintf("does not fit: %.2f GiB > %.2f GiB budget", total, budgetGiB)
	}
	return fit
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
