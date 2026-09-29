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
