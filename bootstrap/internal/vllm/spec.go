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
)

type Spec struct {
	Image      string   `yaml:"image"`
	Model      Model    `yaml:"model"`
	ServedName string   `yaml:"servedName"`
	Port       int      `yaml:"port"`
	Args       []string `yaml:"args"`
	HealthGate Gate     `yaml:"healthGate"`
}

type Model struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision"`
}

type Gate struct {
	Timeout time.Duration `yaml:"timeout"`
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

	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port must be 1–65535, got %d: the spec defines what runs on the host", s.Port)
	}

	if s.HealthGate.Timeout <= 0 {
		return fmt.Errorf("healthGate.timeout must be > 0, got %v: the spec defines what runs on the host", s.HealthGate.Timeout)
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

		if forbiddenFlags[flagName] {
			if flagName == "--model" {
				return fmt.Errorf("args must not contain %s: model.repo is already passed positionally by talops's rendered Exec= line", flagName)
			}
			return fmt.Errorf("args must not contain %s: talops's rendered Exec= line already sets it", flagName)
		}
	}

	return nil
}
