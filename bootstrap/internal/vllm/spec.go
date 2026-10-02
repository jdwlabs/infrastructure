// Package vllm converges the GPU host's model server to the definition in inference/vllm/serving.yaml.
package vllm

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/hostconverge"
)

type Spec struct {
	Image      string `yaml:"image"`
	Model      Model  `yaml:"model"`
	ServedName string `yaml:"servedName"`
	// ServedAliases are further names the server answers to, so a consumer
	// still requesting an old name keeps working while it moves to ServedName.
	ServedAliases []string `yaml:"servedAliases"`
	Port          int      `yaml:"port"`
	Args          []string `yaml:"args"`
	HealthGate    Gate     `yaml:"healthGate"`
}

type Model struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision"`
}

type Gate struct {
	Timeout time.Duration `yaml:"timeout"`
}

// rollbackSlack is the part of hostconverge.RollbackTimeout reserved for
// restoring the previous unit and restarting it before the gate re-runs.
const rollbackSlack = 5 * time.Minute

// MaxGateTimeout is the longest healthGate.timeout a spec may set: a longer
// gate would be cut short on rollback, reporting a restored server that is
// still loading as not serving.
func MaxGateTimeout() time.Duration {
	return hostconverge.RollbackTimeout - rollbackSlack
}

func Load(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	return Parse(data)
}

func Parse(data []byte) (Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Spec{}, err
	}

	if s.Port == 0 {
		s.Port = 8000
	}
	if s.HealthGate.Timeout == 0 {
		s.HealthGate.Timeout = 10 * time.Minute
	}

	if err := validate(s); err != nil {
		return Spec{}, err
	}

	return s, nil
}

func (s Spec) ImageDigest() string {
	idx := strings.Index(s.Image, "@sha256:")
	if idx >= 0 {
		return s.Image[idx+1:]
	}
	return ""
}

func validate(s Spec) error {
	// Restricted to the OCI reference charset (no ; $ ` or other shell
	// metacharacters): remote.go interpolates this value into commands it
	// runs as root over SSH, so a character the regex would let through is
	// a root shell injection, not just a malformed reference.
	imageRegex := regexp.MustCompile(`^[a-z0-9.:/_-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	if !imageRegex.MatchString(s.Image) {
		return fmt.Errorf("image must be <repo>:<tag>@sha256:<64 hex>, got %q: the digest pins what runs and the tag is what Renovate reads", s.Image)
	}

	revRegex := regexp.MustCompile("^[0-9a-f]{40}$")
	if !revRegex.MatchString(s.Model.Revision) {
		return fmt.Errorf("model.revision must be a 40-character commit, got %q: a branch moves, so it cannot pin what serves", s.Model.Revision)
	}

	// Model.Repo, servedName and args all end up as literal words in the
	// Quadlet's Exec=, a systemd command line that splits on whitespace and
	// quotes and expands % and $ — and servedName also lands unescaped in a
	// Prometheus label the drift check emits. Restricting to this charset
	// keeps both safe without either side needing to quote or escape.
	safeTokenRegex := regexp.MustCompile(`^[A-Za-z0-9._:/=,+@-]+$`)

	if !safeTokenRegex.MatchString(s.Model.Repo) {
		return fmt.Errorf("model.repo must match %s, got %q: Exec= is a systemd command line that splits on whitespace and quotes and expands %% and $", safeTokenRegex.String(), s.Model.Repo)
	}
	if strings.HasPrefix(s.Model.Repo, "-") {
		return fmt.Errorf("model.repo must not start with '-', got %q: it is vllm serve's first argument, and a leading dash would make it a flag", s.Model.Repo)
	}

	if s.ServedName == "" {
		return fmt.Errorf("servedName must not be empty: it identifies the model in client requests")
	}
	if !safeTokenRegex.MatchString(s.ServedName) {
		return fmt.Errorf("servedName must match %s, got %q: Exec= is a systemd command line that splits on whitespace and quotes and expands %% and $", safeTokenRegex.String(), s.ServedName)
	}
	// --served-model-name takes one or more values, so a name starting with
	// '-' would end the list and be parsed as the next option instead.
	if strings.HasPrefix(s.ServedName, "-") {
		return fmt.Errorf("servedName must not start with '-', got %q: --served-model-name would read it as the next option, not a name", s.ServedName)
	}

	seen := map[string]bool{s.ServedName: true}
	for _, alias := range s.ServedAliases {
		if !safeTokenRegex.MatchString(alias) {
			return fmt.Errorf("servedAliases must each match %s, got %q: Exec= is a systemd command line that splits on whitespace and quotes and expands %% and $", safeTokenRegex.String(), alias)
		}
		if strings.HasPrefix(alias, "-") {
			return fmt.Errorf("servedAliases must not start with '-', got %q: --served-model-name would read it as the next option, not a name", alias)
		}
		if alias == s.ServedName {
			return fmt.Errorf("servedAliases must not repeat servedName %q: it is already the name the server answers to and reports", alias)
		}
		if seen[alias] {
			return fmt.Errorf("servedAliases has a duplicate %q: each name is served once, so a repeat is a typo for another name", alias)
		}
		seen[alias] = true
	}

	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port must be 1–65535, got %d: the spec defines what runs on the host", s.Port)
	}

	if s.HealthGate.Timeout <= 0 {
		return fmt.Errorf("healthGate.timeout must be > 0, got %v: the spec defines what runs on the host", s.HealthGate.Timeout)
	}
	if limit := MaxGateTimeout(); s.HealthGate.Timeout > limit {
		return fmt.Errorf("healthGate.timeout must be <= %v, got %v: a rollback re-runs the gate against the restored server inside the %v rollback bound, after restore and restart", limit, s.HealthGate.Timeout, hostconverge.RollbackTimeout)
	}

	forbiddenFlags := map[string]bool{
		"--model":             true,
		"--port":              true,
		"--served-model-name": true,
		"--host":              true,
		"--revision":          true,
	}

	for _, arg := range s.Args {
		if !safeTokenRegex.MatchString(arg) {
			return fmt.Errorf("args must match %s, got %q: Exec= is a systemd command line that splits on whitespace and quotes and expands %% and $", safeTokenRegex.String(), arg)
		}

		var flagName string
		if eqIdx := strings.Index(arg, "="); eqIdx >= 0 {
			flagName = arg[:eqIdx]
		} else {
			flagName = arg
		}
		// FlexibleArgumentParser (vllm/utils/argparse_utils.py, v0.24.0)
		// rewrites --root.key=value into --root '{"key": value}' before
		// argparse parses it, so the flag a dotted option sets is its root.
		if dotIdx := strings.Index(flagName, "."); dotIdx >= 0 && strings.HasPrefix(flagName, "--") {
			flagName = flagName[:dotIdx]
		}

		// vLLM's FlexibleArgumentParser normalises '_' to '-' in long-option
		// names, so --served_model_name is the same flag to vLLM as
		// --served-model-name; look it up the way vLLM would, not the way
		// it was spelled here.
		canonical := strings.ReplaceAll(flagName, "_", "-")

		matched := canonical
		if !forbiddenFlags[matched] && canonical != "--" && strings.HasPrefix(canonical, "--") {
			// vllm/utils/argparse_utils.py:113-134 subclasses
			// argparse.ArgumentParser without allow_abbrev=False, so
			// argparse resolves any unambiguous prefix of a long option to
			// the full option - an abbreviated managed flag would still
			// override it. "--" alone is argparse's end-of-options marker,
			// not an abbreviation, so it's excluded above.
			for flag := range forbiddenFlags {
				if len(canonical) < len(flag) && strings.HasPrefix(flag, canonical) {
					matched = flag
					break
				}
			}
		}

		if forbiddenFlags[matched] {
			if matched == "--served-model-name" {
				return fmt.Errorf("args must not contain %s: talops's rendered Exec= line sets it from servedName, so add a further name to servedAliases instead", flagName)
			}
			if matched == "--model" {
				return fmt.Errorf("args must not contain %s: model.repo is already passed positionally by talops's rendered Exec= line", flagName)
			}
			return fmt.Errorf("args must not contain %s: talops's rendered Exec= line already sets it", flagName)
		}
	}

	return nil
}
