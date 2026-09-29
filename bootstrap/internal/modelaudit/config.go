// Package modelaudit finds newly released models that could replace the one
// inference/vllm/serving.yaml serves, and reports them without changing
// anything but a Jira ticket.
package modelaudit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is inference/vllm/audit.yaml.
type Config struct {
	WindowDays         int      `yaml:"windowDays"`
	TrendingWindowDays int      `yaml:"trendingWindowDays"`
	TrendingN          int      `yaml:"trendingN"`
	MaxCandidates      int      `yaml:"maxCandidates"`
	MaxRequests        int      `yaml:"maxRequests"`
	DiscoveryRequests  int      `yaml:"discoveryRequests"`
	GPUMemMiB          int      `yaml:"gpuMemMiB"`
	OverheadGiB        float64  `yaml:"overheadGiB"`
	Orgs               []string `yaml:"orgs"`
	Licenses           Licenses `yaml:"licenses"`
	Parsers            []Rule   `yaml:"parsers"`
}

type Licenses struct {
	Allow      []string `yaml:"allow"`
	AllowNames []string `yaml:"allowNames"`
}

// Rule maps a model family to the vLLM tool parser its chat format needs.
type Rule struct {
	ModelType string   `yaml:"modelType"`
	NameRegex string   `yaml:"nameRegex"`
	Parser    string   `yaml:"parser"`
	ExtraArgs []string `yaml:"extraArgs"`

	nameRe *regexp.Regexp
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	intRange := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			errs = append(errs, fmt.Errorf("%s must be %d–%d, got %d", name, lo, hi, v))
		}
	}
	intRange("windowDays", c.WindowDays, 1, 90)
	intRange("trendingWindowDays", c.TrendingWindowDays, 1, 365)
	intRange("trendingN", c.TrendingN, 1, 100)
	intRange("maxCandidates", c.MaxCandidates, 1, 50)
	// The Hub allows 500 anonymous requests per 5 minutes per IP; a cap above
	// that could only be reached by being rate limited.
	intRange("maxRequests", c.MaxRequests, 1, 500)
	intRange("discoveryRequests", c.DiscoveryRequests, 1, c.MaxRequests-1)
	intRange("gpuMemMiB", c.GPUMemMiB, 1024, 1<<20)
	if c.OverheadGiB < 0 || c.OverheadGiB > 64 {
		errs = append(errs, fmt.Errorf("overheadGiB must be 0–64, got %v", c.OverheadGiB))
	}
	if len(c.Orgs) == 0 {
		errs = append(errs, errors.New("orgs must name at least one organisation"))
	}
	seen := map[string]bool{}
	for _, o := range c.Orgs {
		if o == "" || seen[o] {
			errs = append(errs, fmt.Errorf("orgs entries must be non-empty and unique, got %q", o))
		}
		seen[o] = true
	}
	if len(c.Parsers) == 0 {
		errs = append(errs, errors.New("parsers must hold at least one rule"))
	}
	for i := range c.Parsers {
		r := &c.Parsers[i]
		if r.ModelType == "" || r.Parser == "" {
			errs = append(errs, fmt.Errorf("parsers[%d] needs modelType and parser", i))
		}
		if r.NameRegex != "" {
			re, err := regexp.Compile(r.NameRegex)
			if err != nil {
				errs = append(errs, fmt.Errorf("parsers[%d].nameRegex: %w", i, err))
				continue
			}
			r.nameRe = re
		}
	}
	return errors.Join(errs...)
}

// ResolveParser returns the first rule matching the repo. The top-level
// model_type is tried first; multimodal wrappers such as Mistral3 name their
// language model only in text_config, so that is the fallback.
func (c Config) ResolveParser(repo, modelType, textModelType string) (Rule, bool) {
	for _, mt := range []string{modelType, textModelType} {
		if mt == "" {
			continue
		}
		for _, r := range c.Parsers {
			if r.ModelType != mt {
				continue
			}
			if r.nameRe != nil && !r.nameRe.MatchString(repo) {
				continue
			}
			return r, true
		}
	}
	return Rule{}, false
}
