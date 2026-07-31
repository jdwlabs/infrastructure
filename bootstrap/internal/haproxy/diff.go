package haproxy

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines surround each hunk. Three matches
// what every other unified diff prints, so the output reads without a legend.
const diffContext = 3

// Diff renders a unified diff from the deployed config to the freshly rendered
// one. It returns an empty string when the two are equivalent.
//
// Both sides are normalised first so a trailing-newline or CRLF difference
// introduced by the write/read round-trip cannot be reported as config drift.
func Diff(deployed, rendered string) string {
	if normalizeConfig(deployed) == normalizeConfig(rendered) {
		return ""
	}

	from := splitLines(deployed)
	to := splitLines(rendered)
	ops := diffOps(from, to)

	var b strings.Builder
	b.WriteString("--- deployed " + ConfigPath + "\n")
	b.WriteString("+++ rendered\n")
	for _, hunk := range hunks(ops) {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
			hunk.fromStart, hunk.fromCount, hunk.toStart, hunk.toCount)
		for _, op := range hunk.ops {
			b.WriteString(op.marker())
			b.WriteString(op.line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Truncate caps a diff for display and reports whether it was shortened, so a
// large diff costs a preview rather than the whole file — and the caller can
// tell the reader that a longer version exists.
func Truncate(diff string, limit int) (string, bool) {
	if limit <= 0 || len(diff) <= limit {
		return diff, false
	}
	cut := diff[:limit]
	// Cut on a line boundary: half a diff line reads as a different change.
	if idx := strings.LastIndex(cut, "\n"); idx > 0 {
		cut = cut[:idx+1]
	}
	return cut, true
}

type diffKind int

const (
	opEqual diffKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind diffKind
	line string
}

func (o diffOp) marker() string {
	switch o.kind {
	case opDelete:
		return "-"
	case opInsert:
		return "+"
	default:
		return " "
	}
}

func splitLines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// diffOps walks a longest-common-subsequence table backwards into an ordered
// edit script. A generated HAProxy config is a couple of hundred lines, so the
// quadratic table is cheaper than pulling in a diff dependency.
func diffOps(from, to []string) []diffOp {
	lcs := make([][]int, len(from)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(to)+1)
	}
	for i := len(from) - 1; i >= 0; i-- {
		for j := len(to) - 1; j >= 0; j-- {
			if from[i] == to[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < len(from) && j < len(to) {
		switch {
		case from[i] == to[j]:
			ops = append(ops, diffOp{opEqual, from[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{opDelete, from[i]})
			i++
		default:
			ops = append(ops, diffOp{opInsert, to[j]})
			j++
		}
	}
	for ; i < len(from); i++ {
		ops = append(ops, diffOp{opDelete, from[i]})
	}
	for ; j < len(to); j++ {
		ops = append(ops, diffOp{opInsert, to[j]})
	}
	return ops
}

type diffHunk struct {
	fromStart, fromCount int
	toStart, toCount     int
	ops                  []diffOp
}

// hunks groups the edit script into unified-diff hunks, dropping runs of
// unchanged lines longer than twice the context window.
func hunks(ops []diffOp) []diffHunk {
	changed := make([]int, 0, len(ops))
	for i, op := range ops {
		if op.kind != opEqual {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	var result []diffHunk
	start := max(changed[0]-diffContext, 0)
	end := min(changed[0]+diffContext+1, len(ops))

	for _, idx := range changed[1:] {
		if idx-diffContext > end {
			result = append(result, buildHunk(ops, start, end))
			start = idx - diffContext
		}
		end = min(idx+diffContext+1, len(ops))
	}
	return append(result, buildHunk(ops, start, end))
}

func buildHunk(ops []diffOp, start, end int) diffHunk {
	fromLine, toLine := 1, 1
	for _, op := range ops[:start] {
		if op.kind != opInsert {
			fromLine++
		}
		if op.kind != opDelete {
			toLine++
		}
	}

	hunk := diffHunk{fromStart: fromLine, toStart: toLine, ops: ops[start:end]}
	for _, op := range hunk.ops {
		if op.kind != opInsert {
			hunk.fromCount++
		}
		if op.kind != opDelete {
			hunk.toCount++
		}
	}
	return hunk
}
