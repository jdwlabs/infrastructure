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
			"repo":  "Org/M",
			"rev":   strings.Repeat("b", 40),
			"name":  "qwen/qwen3-coder-30b-a3b",
			"args":  "[--quantization=awq_marlin]",
		}
	}
	render := func(m map[string]string) []byte {
		return []byte(fmt.Sprintf("image: %s\nmodel: {repo: %s, revision: %s}\nservedName: %s\nargs: %s\n",
			m["image"], m["repo"], m["rev"], m["name"], m["args"]))
	}
	cases := map[string]func(map[string]string){
		"tag without digest":          func(m map[string]string) { m["image"] = "docker.io/vllm/vllm-openai:v0.24.0" },
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
		"--served_model_name, underscore spelling": func(m map[string]string) {
			m["args"] = "[--served_model_name=y]"
		},
		"--host in args":     func(m map[string]string) { m["args"] = "[--host=0.0.0.0]" },
		"--revision in args": func(m map[string]string) { m["args"] = "[--revision=main]" },
		"--revision as a separate word": func(m map[string]string) {
			m["args"] = "[--revision, main]"
		},
		"model repo that is a flag": func(m map[string]string) { m["repo"] = "--trust-remote-code" },
		"image contains semicolon": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0;id@sha256:" + strings.Repeat("a", 64)
		},
		"image contains dollar": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0$(id)@sha256:" + strings.Repeat("a", 64)
		},
		"image contains backtick": func(m map[string]string) {
			m["image"] = "docker.io/vllm/vllm-openai:v0.24.0`id`@sha256:" + strings.Repeat("a", 64)
		},
	}
	_, err := Parse(render(good()))
	require.NoError(t, err, "the base every case mutates must itself be valid")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := good()
			mutate(m)
			_, err := Parse(render(m))
			assert.Error(t, err)
		})
	}
}

// TestParseRejectsModelFlagWithPositionalMessage locks in --model's own
// error message: model.repo is passed positionally by ExecArgs, not via a
// --model flag, so reusing the generic "already sets it" wording (accurate
// for --port/--served-model-name/--host, which are real flags) would be
// misleading for this one.
func TestParseRejectsModelFlagWithPositionalMessage(t *testing.T) {
	m := map[string]string{
		"image": "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("a", 64),
		"rev":   strings.Repeat("b", 40),
		"name":  "qwen/qwen3-coder-30b-a3b",
		"args":  "[--model=x]",
	}
	data := []byte(fmt.Sprintf("image: %s\nmodel: {repo: Org/M, revision: %s}\nservedName: %s\nargs: %s\n",
		m["image"], m["rev"], m["name"], m["args"]))

	_, err := Parse(data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model.repo is already passed positionally")
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

func TestParseBoundsGateTimeoutByRollback(t *testing.T) {
	require.Equal(t, 10*time.Minute, MaxGateTimeout())
	parse := func(timeout string) error {
		_, err := Parse([]byte(`image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
healthGate: {timeout: ` + timeout + `}
`))
		return err
	}

	assert.NoError(t, parse("10m"))

	err := parse("10m1s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "healthGate.timeout")
	assert.Contains(t, err.Error(), "10m0s")
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

// yamlSingleQuote wraps a value in YAML single-quoted scalar syntax, whose
// only escape is a doubled quote for a literal one — so a space, ", \, %,
// or $ inside it reaches the decoded Go string unmolested, and it's our own
// validate() regex (not a YAML-parse failure) that has to reject it.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestParseRejectsUnsafeCharacters(t *testing.T) {
	type fields struct {
		repo string
		name string
		arg  string
	}
	good := fields{
		repo: "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ",
		name: "qwen/qwen3-coder-30b-a3b",
		arg:  "--quantization=awq_marlin",
	}
	render := func(f fields) []byte {
		return []byte(fmt.Sprintf(
			"image: docker.io/vllm/vllm-openai:v0.24.0@sha256:%s\nmodel: {repo: %s, revision: %s}\nservedName: %s\nargs: [%s]\n",
			strings.Repeat("a", 64), yamlSingleQuote(f.repo), strings.Repeat("b", 40), yamlSingleQuote(f.name), yamlSingleQuote(f.arg),
		))
	}

	t.Run("valid fields still parse", func(t *testing.T) {
		_, err := Parse(render(good))
		assert.NoError(t, err)
	})

	chars := map[string]string{
		"space":        " ",
		"double quote": `"`,
		"single quote": "'",
		"backslash":    `\`,
		"percent":      "%",
		"dollar":       "$",
	}
	for charName, ch := range chars {
		t.Run("model.repo contains "+charName, func(t *testing.T) {
			f := good
			f.repo = "Org/M" + ch + "odel"
			_, err := Parse(render(f))
			assert.Error(t, err)
		})
		t.Run("servedName contains "+charName, func(t *testing.T) {
			f := good
			f.name = "qwen" + ch + "ai"
			_, err := Parse(render(f))
			assert.Error(t, err)
		})
		t.Run("args contains "+charName, func(t *testing.T) {
			f := good
			f.arg = "--foo=bar" + ch + "baz"
			_, err := Parse(render(f))
			assert.Error(t, err)
		})
	}
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

// CI fails if the committed serving.yaml ever stops loading or validating.
// It deliberately doesn't freeze values: model and image changes arrive as
// reviewed PRs, including Renovate's, and this test must not force an edit
// on every one of them.
func TestCommittedServingSpecIsValid(t *testing.T) {
	_, err := Load("../../../inference/vllm/serving.yaml")
	require.NoError(t, err)
}

func aliasSpecYAML(servedName, aliases string) []byte {
	return []byte(`image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: ` + servedName + `
servedAliases: ` + aliases + `
`)
}

func TestParseAcceptsServedAliases(t *testing.T) {
	s, err := Parse(aliasSpecYAML("local-chat", "[qwen/qwen3-coder-30b-a3b, 'old:name=v1,+@']"))
	require.NoError(t, err)
	assert.Equal(t, []string{"qwen/qwen3-coder-30b-a3b", "old:name=v1,+@"}, s.ServedAliases)
}

func TestParseServedAliasesIsOptional(t *testing.T) {
	s, err := Parse(aliasSpecYAML("local-chat", "[]"))
	require.NoError(t, err)
	assert.Empty(t, s.ServedAliases)
}

func TestParseRejectsBadServedAliases(t *testing.T) {
	cases := map[string]struct {
		aliases string
		wantMsg string
	}{
		"duplicate alias":           {"[qwen/a, qwen/a]", "duplicate"},
		"alias equal to servedName": {"[local-chat]", "servedName"},
		"empty alias":               {`[""]`, "servedAliases"},
		"alias that is a flag":      {"['--trust-remote-code']", "'-'"},
		"alias with space":          {"['qwen a']", "servedAliases"},
		"alias with dollar":         {"['qwen$a']", "servedAliases"},
		"alias with percent":        {"['qwen%a']", "servedAliases"},
		"alias with double quote":   {`['qwen"a']`, "servedAliases"},
		"alias with single quote":   {`['qwen''a']`, "servedAliases"},
		"alias with backslash":      {`['qwen\a']`, "servedAliases"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(aliasSpecYAML("local-chat", tc.aliases))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// --served-model-name is nargs="+" in vLLM, so a servedName that starts with
// '-' would be read as the next option rather than as a name.
func TestParseRejectsServedNameThatIsAFlag(t *testing.T) {
	_, err := Parse(aliasSpecYAML("'--trust-remote-code'", "[]"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'-'")
}

// A second name is a legitimate need, so the refusal names the field that
// meets it rather than only saying the flag is taken.
func TestParseRejectsServedModelNameFlagPointingAtAliases(t *testing.T) {
	for _, arg := range []string{"--served-model-name=y", "--served_model_name=y", "--served-model-name"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Parse([]byte(`image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: local-chat
args: ['` + arg + `']
`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "servedAliases")
		})
	}
}

// vLLM's FlexibleArgumentParser (vllm/utils/argparse_utils.py:113-134)
// subclasses argparse.ArgumentParser without allow_abbrev=False, so argparse
// resolves any unambiguous prefix of a long option to the full option - an
// abbreviated managed flag would still override --served-model-name, --model,
// --revision, --port or --host.
func TestParseRejectsAbbreviatedManagedFlags(t *testing.T) {
	cases := map[string]string{
		"served-model-name, abbreviated with =":       "[--served-model-nam=other]",
		"served_model_name, abbreviated + underscore": "[--served_model_n, x]",
		"revision, abbreviated":                       "[--revisio, x]",
		"model, abbreviated":                          "[--mod, x]",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			data := []byte(`image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: ` + args + `
`)
			_, err := Parse(data)
			require.Error(t, err)
		})
	}
}

// FlexibleArgumentParser rewrites --root.key=value into --root '{"key": ...}'
// before argparse sees it, so a dotted form of a managed flag still sets it.
func TestParseRejectsDottedManagedFlags(t *testing.T) {
	cases := map[string]string{
		"served-model-name, dotted":     "[--served-model-name.foo=x]",
		"model, dotted, separate value": "[--model.foo, x]",
		"revision, dotted":              "[--revision.foo=x]",
		"abbreviated and dotted":        "[--served-model-nam.foo=x]",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			data := []byte(`image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: ` + args + `
`)
			_, err := Parse(data)
			require.Error(t, err)
		})
	}
}

// A dotted option whose root is not managed is ordinary vLLM configuration.
func TestParseAcceptsDottedUnmanagedFlags(t *testing.T) {
	_, err := Parse([]byte(`
image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: [--compilation-config.level=3]
`))
	require.NoError(t, err)
}

// --max-model-len is not a prefix of any forbidden flag, so the prefix check
// must not reject it.
func TestParseAcceptsFlagsThatAreNotAbbreviations(t *testing.T) {
	s, err := Parse([]byte(`
image: docker.io/vllm/vllm-openai:v0.24.0@sha256:` + strings.Repeat("a", 64) + `
model: {repo: Org/M, revision: ` + strings.Repeat("b", 40) + `}
servedName: qwen/qwen3-coder-30b-a3b
args: [--max-model-len=32768]
`))
	require.NoError(t, err)
	assert.Equal(t, []string{"--max-model-len=32768"}, s.Args)
}
