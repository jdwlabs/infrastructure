package modelaudit

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
)

var servablePipelines = map[string]bool{"text-generation": true, "image-text-to-text": true}

// PreFilter rejects on list data alone, so the three enrichment requests per
// repo are only spent on plausible ones. It returns the resolved licence, or
// the first failing step's reason.
func PreFilter(it hub.ListItem, cfg Config, hasToken bool) (license, reason string) {
	if it.SHA == "" {
		return "", "no revision sha"
	}

	pt := it.PipelineTag
	if pt == "" {
		pt = string(it.CardData.PipelineTag)
	}
	switch {
	case pt == "":
		return "", "pipeline_tag missing"
	case !servablePipelines[pt]:
		return "", "pipeline_tag " + pt
	}

	license = string(it.CardData.License)
	if license == "" {
		for _, tag := range it.Tags {
			if id, ok := strings.CutPrefix(tag, "license:"); ok {
				license = id
				break
			}
		}
	}
	switch {
	case license == "":
		return "", "license missing"
	case license == "other":
		name := string(it.CardData.LicenseName)
		if !slices.Contains(cfg.Licenses.AllowNames, name) {
			if name == "" {
				name = "unnamed"
			}
			return "", fmt.Sprintf("license other (%s) not allowed", name)
		}
		license = "other:" + name
	case !slices.Contains(cfg.Licenses.Allow, license):
		return "", fmt.Sprintf("license %s not allowed", license)
	}

	if it.Gated != "" && !hasToken {
		return "", fmt.Sprintf("gated (%s): licence acceptance and HF_TOKEN required", it.Gated)
	}
	return license, ""
}

var instructNameRe = regexp.MustCompile(`(?i)(instruct|chat|-it\b)`)

// InstructionTuned accepts any one signal: base models answer tool calls
// with prose, which the health gate would only catch after a restart.
func InstructionTuned(id string, info hub.ModelInfo, tree []hub.TreeEntry) bool {
	name := id[strings.LastIndex(id, "/")+1:]
	if instructNameRe.MatchString(name) || slices.Contains(info.Tags, "conversational") || info.HasChatTemplate() {
		return true
	}
	return slices.ContainsFunc(tree, func(e hub.TreeEntry) bool { return e.Path == "chat_template.jinja" })
}

// ModelTypes returns config.json's model_type and text_config.model_type.
func ModelTypes(cfg map[string]any) (top, text string) {
	top, _ = cfg["model_type"].(string)
	if tc, ok := cfg["text_config"].(map[string]any); ok {
		text, _ = tc["model_type"].(string)
	}
	return top, text
}

// ResolveCandidateParser applies the parser rules and checks the result
// against the registry of the vLLM release that would serve it.
func ResolveCandidateParser(cfg Config, reg Registry, tag, id string, conf map[string]any) (Rule, string) {
	top, text := ModelTypes(conf)
	rule, ok := cfg.ResolveParser(id, top, text)
	if !ok {
		mt := top
		if mt == "" {
			mt = "(none)"
		}
		return Rule{}, "no parser rule for " + mt
	}
	if !reg[rule.Parser] {
		return Rule{}, fmt.Sprintf("parser %s not in vLLM %s", rule.Parser, tag)
	}
	return rule, ""
}

// CheckArchitecture passes a config when any of its architectures is
// registered, because vLLM tries each in turn and loads the first it knows
// (vllm/model_executor/models/registry.py:1207-1211, 1261-1265). The
// Transformers-backend fallback for an unregistered architecture is not
// counted: whether it can serve a model is not knowable from the config, and
// a rejection only asks a human to look.
func CheckArchitecture(archs Architectures, tag string, conf map[string]any) string {
	listed := architectures(conf)
	if len(listed) == 0 {
		return "architecture missing"
	}
	for _, a := range listed {
		if archs[a] {
			return ""
		}
	}
	return fmt.Sprintf("architecture %s not in vLLM %s", listed[0], tag)
}
