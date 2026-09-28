package vllm

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAcceptsAPinnedSpecAndDefaults(t *testing.T) {
	s, err := Parse([]byte(`
image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: [--quantization=awq_marlin, --max-model-len=32768]
`))
	require.NoError(t, err)
	assert.Equal(t, 8000, s.Port)
	assert.Equal(t, 10*time.Minute, s.HealthGate.Timeout)
	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), s.ImageDigest())
}

func TestParseRejects(t *testing.T) {
	good := func() map[string]string {
		return map[string]string{
			"image": "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("a", 64),
			"rev":   strings.Repeat("b", 40),
			"name":  "qwen/qwen3-coder-30b-a3b",
			"args":  "[--quantization=awq_marlin]",
		}
	}
	render := func(m map[string]string) []byte {
		return []byte(fmt.Sprintf("image: %s\nmodel: {repo: Org/M, revision: %s}\nservedName: %s\nargs: %s\n",
			m["image"], m["rev"], m["name"], m["args"]))
	}
	cases := map[string]func(map[string]string){
		"tag without digest":          func(m map[string]string) { m["image"] = "docker.io/vllm/vllm-openai:v0.24.0" },
		"digest without tag":          func(m map[string]string) { m["image"] = "@sha256:" + strings.Repeat("a", 64) },
		"digest without repo":         func(m map[string]string) { m["image"] = "@sha256:" + strings.Repeat("a", 64) },
		"repo without tag":            func(m map[string]string) { m["image"] = "docker.io/vllm/vllm-openai@sha256:" + strings.Repeat("a", 64) },
		"ported registry without tag": func(m map[string]string) { m["image"] = "registry:5000/x@sha256:" + strings.Repeat("a", 64) },
		"digest 40 hex chars": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("a", 40)
		},
		"digest uppercase hex": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("A", 64)
		},
		"digest non-hex": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("z", 64)
		},
		"branch as revision":  func(m map[string]string) { m["rev"] = "main" },
		"short revision":      func(m map[string]string) { m["rev"] = "abc123" },
		"empty served name":   func(m map[string]string) { m["name"] = `""` },
		"--model in args":     func(m map[string]string) { m["args"] = "[--model=x]" },
		"--port in args":      func(m map[string]string) { m["args"] = "[--port, '9000']" },
		"--served-model-name": func(m map[string]string) { m["args"] = "[--served-model-name=y]" },
		"--host in args":      func(m map[string]string) { m["args"] = "[--host=0.0.0.0]" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := good()
			mutate(m)
			_, err := Parse(render(m))
			assert.Error(t, err)
		})
	}
}

func TestParseRejectsMalformedYAML(t *testing.T) {
	cases := map[string]string{
		"healthgate lowercase": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
healthgate: {timeout: 10m}
`,
		"model typo reop": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, reop: main, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(yaml))
			assert.Error(t, err)
		})
	}
}

func TestParseRejectsOutOfRangeValues(t *testing.T) {
	cases := map[string]string{
		"port -1": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
port: -1
`,
		"port 70000": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
port: 70000
`,
		"timeout -5m": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
healthGate: {timeout: -5m}
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(yaml))
			assert.Error(t, err)
		})
	}
}

func TestParseAcceptsNonForbiddenFlags(t *testing.T) {
	s, err := Parse([]byte(`
image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: [--model-loader-extra-config=foo, --quantization=awq_marlin]
`))
	require.NoError(t, err)
	assert.Equal(t, 8000, s.Port)
}

func TestParseAcceptsValidImages(t *testing.T) {
	cases := map[string]string{
		"docker.io with tag": `image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
`,
		"ported registry with tag": `image: registry:5000/x:tag@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
`,
		"localhost with tag": `image: localhost:5000/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(yaml))
			assert.NoError(t, err)
		})
	}
}
