package modelaudit

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Registry is the set of tool-parser names a vLLM release accepts for
// --tool-call-parser.
type Registry map[string]bool

// minRegistryKeys is a floor, not an expectation: v0.24.0 has 43. Fewer than
// this means the extraction matched something other than the parser table.
const minRegistryKeys = 10

var registryKeyRe = regexp.MustCompile(`^\s{4}"([^"]+)":`)

// ParseRegistry reads the keys of _TOOL_PARSERS_TO_REGISTER from
// vllm/tool_parsers/__init__.py. It reads Python as text, so it is strict:
// anything unexpected is an error rather than a partial registry.
func ParseRegistry(src []byte, incumbentParser string) (Registry, error) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	reg := Registry{}
	inTable, closed := false, false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !inTable {
			if strings.HasPrefix(line, "_TOOL_PARSERS_TO_REGISTER = {") {
				inTable = true
			}
			continue
		}
		if line == "}" {
			closed = true
			break
		}
		if m := registryKeyRe.FindStringSubmatch(line); m != nil {
			reg[m[1]] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read tool-parser registry: %w", err)
	}
	if !inTable || !closed {
		return nil, fmt.Errorf("tool-parser registry: _TOOL_PARSERS_TO_REGISTER table not found")
	}
	if len(reg) < minRegistryKeys {
		return nil, fmt.Errorf("tool-parser registry: %d keys, want at least %d", len(reg), minRegistryKeys)
	}
	if !reg[incumbentParser] {
		return nil, fmt.Errorf("tool-parser registry lacks the incumbent's parser %q", incumbentParser)
	}
	return reg, nil
}

// Architectures is the set of config.json architectures a vLLM release
// serves natively.
type Architectures map[string]bool

// minArchitectures is a floor, not an expectation: v0.24.0 has 359.
const minArchitectures = 100

// anchorArchitecture must be present in any real registry. It is not the
// incumbent's architecture, which serving.yaml does not record and reading
// would cost a request; Llama is the reference model vLLM has registered
// since its first release, so its absence means the extraction broke.
const anchorArchitecture = "LlamaForCausalLM"

var (
	composedTableRe = regexp.MustCompile(`^\s{4}\*\*(_[A-Z0-9_]+),$`)
	tableStartRe    = regexp.MustCompile(`^(_[A-Z0-9_]+) = \{$`)
)

// ParseModelRegistry reads the keys of every table _VLLM_MODELS spreads in
// vllm/model_executor/models/registry.py. A key is a line indented exactly
// four spaces that opens with a quoted name and a colon; an entry split
// over several lines still opens that way, and its module and class values
// are indented deeper. The tables are read only up to their closing brace at
// column 0, so the _PREVIOUSLY_SUPPORTED_MODELS and _OOT_SUPPORTED_MODELS
// names, which vLLM refuses to load, are never read. Like ParseRegistry it
// reads Python as text and fails rather than return a partial set.
func ParseModelRegistry(src []byte) (Architectures, error) {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	tables := map[string][]string{}
	for i, line := range lines {
		if m := tableStartRe.FindStringSubmatch(line); m != nil {
			tables[m[1]] = lines[i+1:]
		}
	}

	body, ok := tables["_VLLM_MODELS"]
	if !ok {
		return nil, fmt.Errorf("model registry: _VLLM_MODELS table not found")
	}
	var composed []string
	closed := false
	for _, line := range body {
		if line == "}" {
			closed = true
			break
		}
		m := composedTableRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("model registry: unexpected _VLLM_MODELS line %q", line)
		}
		composed = append(composed, m[1])
	}
	if !closed || len(composed) == 0 {
		return nil, fmt.Errorf("model registry: _VLLM_MODELS table not closed or empty")
	}

	archs := Architectures{}
	for _, name := range composed {
		body, ok := tables[name]
		if !ok {
			return nil, fmt.Errorf("model registry: table %s not found", name)
		}
		closed := false
		for _, line := range body {
			if line == "}" {
				closed = true
				break
			}
			// Anything else at column 0 is the next statement: the table
			// ended without the brace this reader relies on.
			if line != "" && line[0] != ' ' && line[0] != '#' {
				break
			}
			if m := registryKeyRe.FindStringSubmatch(line); m != nil {
				archs[m[1]] = true
			}
		}
		if !closed {
			return nil, fmt.Errorf("model registry: table %s not closed", name)
		}
	}
	if len(archs) < minArchitectures {
		return nil, fmt.Errorf("model registry: %d architectures, want at least %d", len(archs), minArchitectures)
	}
	if !archs[anchorArchitecture] {
		return nil, fmt.Errorf("model registry lacks %s", anchorArchitecture)
	}
	return archs, nil
}
