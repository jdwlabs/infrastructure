package modelaudit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/vllm"
)

// Current is what serving.yaml runs today, reduced to what the audit compares
// candidates against.
type Current struct {
	Repo                 string
	Revision             string
	ServedName           string
	ServedAliases        []string
	VLLMTag              string
	ContextTokens        int
	GPUMemoryUtilization float64
	ToolCallParser       string
	Quantization         string
	MaxNumBatchedTokens  int
	// PrefixCachingOff and AsyncSchedulingOff are the two flags that turn
	// off a vLLM default the memory estimate prices.
	PrefixCachingOff   bool
	AsyncSchedulingOff bool
	UnmodelledFlags    []string
	LanguageModelOnly  bool
}

var vllmTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func LoadCurrent(path string) (Current, error) {
	s, err := vllm.Load(path)
	if err != nil {
		return Current{}, err
	}
	return CurrentFromSpec(s)
}

func CurrentFromSpec(s vllm.Spec) (Current, error) {
	ref := s.Image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	tag := ref[strings.LastIndex(ref, ":")+1:]
	// The tag names the vLLM source tree the tool-parser registry is read
	// from, so anything but a release tag would audit against the wrong one.
	if !vllmTagRe.MatchString(tag) {
		return Current{}, fmt.Errorf("image tag %q is not vX.Y.Z: the parser registry is read at that tag", tag)
	}
	c := Current{
		Repo:                 s.Model.Repo,
		Revision:             s.Model.Revision,
		ServedName:           s.ServedName,
		ServedAliases:        s.ServedAliases,
		VLLMTag:              tag,
		GPUMemoryUtilization: 0.90,
	}

	v, ok := argValue(s.Args, "--max-model-len")
	if !ok {
		return Current{}, fmt.Errorf("serving.yaml args carry no --max-model-len: the memory estimate needs the context length")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return Current{}, fmt.Errorf("--max-model-len must be a positive integer, got %q", v)
	}
	c.ContextTokens = n

	if v, ok := argValue(s.Args, "--gpu-memory-utilization"); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return Current{}, fmt.Errorf("--gpu-memory-utilization must be in (0, 1], got %q", v)
		}
		c.GPUMemoryUtilization = f
	}

	// Sliding-window layers hold a window plus one batch of tokens, so the
	// memory estimate needs the batch size vLLM will actually use.
	if v, ok := argValue(s.Args, "--max-num-batched-tokens"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Current{}, fmt.Errorf("--max-num-batched-tokens must be a positive integer, got %q", v)
		}
		c.MaxNumBatchedTokens = n
	}

	c.PrefixCachingOff = hasFlag(s.Args, "--no-enable-prefix-caching")
	c.AsyncSchedulingOff = hasFlag(s.Args, "--no-async-scheduling")
	c.UnmodelledFlags = unmodelledFlags(s.Args)
	c.LanguageModelOnly = hasFlag(s.Args, "--language-model-only")

	p, ok := argValue(s.Args, "--tool-call-parser")
	if !ok || p == "" {
		return Current{}, fmt.Errorf("serving.yaml args carry no --tool-call-parser: the registry sanity check needs the incumbent's parser")
	}
	c.ToolCallParser = p
	c.Quantization, _ = argValue(s.Args, "--quantization")
	return c, nil
}

// unmodelledFlags names the serving flags, set away from the defaults the
// estimate assumes, that change what a linear-attention or sliding layer
// holds: --mamba-cache-mode picks a different number of states per sequence
// than prefix caching's default, the two mamba dtypes resize the state,
// speculative decoding adds conv rows and state blocks, and disabling
// chunked prefill makes a sliding layer hold the whole context. Prefix
// caching and async scheduling are modelled, on or off. The order is fixed
// so the reason a model is rejected with is stable.
func unmodelledFlags(args []string) []string {
	var out []string
	if hasFlag(args, "--no-enable-chunked-prefill") {
		out = append(out, "--no-enable-chunked-prefill")
	}
	if hasFlag(args, "--mamba-cache-mode") {
		out = append(out, "--mamba-cache-mode")
	}
	for _, f := range []string{"--mamba-cache-dtype", "--mamba-ssm-cache-dtype"} {
		if v, ok := argValue(args, f); ok && v != "auto" {
			out = append(out, f)
		}
	}
	if hasFlag(args, "--speculative-config") || hasFlag(args, "-sc") {
		out = append(out, "--speculative-config")
	}
	return out
}

// hasFlag reports a flag in any spelling argValue accepts, value or not.
func hasFlag(args []string, flag string) bool {
	_, ok := argValue(args, flag)
	return ok
}

// argValue reads a flag in either spelling vLLM accepts: --flag=value, or
// --flag and value as two elements. Underscores are normalised the way
// vLLM's own parser does.
func argValue(args []string, flag string) (string, bool) {
	norm := func(s string) string { return strings.ReplaceAll(s, "_", "-") }
	for i, a := range args {
		name, val, hasEq := strings.Cut(a, "=")
		if norm(name) != flag {
			continue
		}
		if hasEq {
			return val, true
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			return args[i+1], true
		}
		return "", true
	}
	return "", false
}
