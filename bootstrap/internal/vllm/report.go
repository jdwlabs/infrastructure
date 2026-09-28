package vllm

import (
	"fmt"
	"strings"
	"time"
)

// ReportStatus renders a StatusResult as TOON.
func ReportStatus(res StatusResult) string {
	var b strings.Builder
	b.WriteString("vllm:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  drift: %t\n", res.Drift)
	fmt.Fprintf(&b, "  legacy: %t\n", res.Legacy)

	fmt.Fprintf(&b, "fields[%d]{name,git,applied,live}:\n", len(res.Fields))
	for _, f := range res.Fields {
		fmt.Fprintf(&b, "  %s,%s,%s,%s\n", toonCell(f.Name), toonCell(f.Git), toonCell(f.Applied), toonCell(f.Live))
	}

	writeList(&b, "notes", res.Notes)
	writeFailure(&b, res.Failure)
	writeList(&b, "help", res.Help)
	return b.String()
}

// ReportPlan renders a PlanResult as TOON. "Nothing to do" is stated rather
// than shown as an absent diff, and a change that the unit diff cannot show
// (a legacy unit, a stale record) says so instead of printing an empty diff.
func ReportPlan(res PlanResult) string {
	var b strings.Builder
	b.WriteString("vllm:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  changed: %t\n", res.Changed)

	writeList(&b, "reasons", res.Reasons)
	switch {
	case res.Failure != nil:
	case !res.Changed:
		b.WriteString("diff: 0 changes — the host already runs what serving.yaml defines\n")
	case res.Diff == "":
		b.WriteString("diff: 0 lines — the unit already matches; apply acts for the reasons above\n")
	default:
		b.WriteString("diff: |\n")
		for _, line := range strings.Split(strings.TrimRight(res.Diff, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	writeList(&b, "consumers", res.Consumers)

	writeFailure(&b, res.Failure)
	writeList(&b, "help", res.Help)
	return b.String()
}

// ReportApply renders an ApplyResult as TOON.
func ReportApply(res ApplyResult) string {
	var b strings.Builder
	b.WriteString("vllm:\n")
	fmt.Fprintf(&b, "  host: %s\n", orDash(res.Host))
	fmt.Fprintf(&b, "  changed: %t\n", res.Changed)
	fmt.Fprintf(&b, "  rolledBack: %t\n", res.RolledBack)
	fmt.Fprintf(&b, "  serving: %s\n", orDash(res.Serving))

	writeList(&b, "reasons", res.Reasons)
	if len(res.Phases) > 0 {
		fmt.Fprintf(&b, "phases[%d]{name,took}:\n", len(res.Phases))
		for _, p := range res.Phases {
			fmt.Fprintf(&b, "  %s,%s\n", toonCell(p.Name), p.Took.Round(time.Millisecond))
		}
	}
	if res.Failure == nil && !res.Changed {
		b.WriteString("result: 0 changes — the host already runs what serving.yaml defines; the server was not restarted\n")
	}

	writeList(&b, "notes", res.Notes)
	writeFailure(&b, res.Failure)
	writeList(&b, "help", res.Help)
	return b.String()
}

func writeList(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s[%d]:\n", key, len(items))
	for _, it := range items {
		fmt.Fprintf(b, "  %s\n", toonCell(it))
	}
}

// writeFailure quotes the message: gate errors are errors.Join results that
// span lines, and a raw newline would end the TOON value mid-message.
func writeFailure(b *strings.Builder, f *Failure) {
	if f == nil {
		return
	}
	fmt.Fprintf(b, "error: {code: %s, msg: %q}\n", f.Code, f.Msg)
}

// toonCell quotes a value that would otherwise break the row's field boundaries.
func toonCell(v string) string {
	if v == "" {
		return "-"
	}
	if strings.ContainsAny(v, ",\"\n") || strings.TrimSpace(v) != v {
		return fmt.Sprintf("%q", v)
	}
	return v
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// diffContext is how many unchanged lines surround each hunk, the width every
// other unified diff uses, so the output reads without a legend.
const diffContext = 3

type diffOp struct {
	mark byte // ' ', '-' or '+'
	line string
}

// lineDiff renders a unified diff from one text to another, or "" when they
// are the same once line endings are normalised — a CRLF or trailing
// newline picked up on the way back from the host is not a unit change.
func lineDiff(from, to, fromLabel, toLabel string) string {
	a, c := splitLines(from), splitLines(to)
	ops := diffOps(a, c)

	var b strings.Builder
	for _, h := range diffHunks(ops) {
		if b.Len() == 0 {
			b.WriteString("--- " + fromLabel + "\n+++ " + toLabel + "\n")
		}
		b.WriteString(h)
	}
	return b.String()
}

func splitLines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// diffOps walks a longest-common-subsequence table into an edit script. A
// Quadlet unit is a few dozen lines, so the quadratic table is cheaper than
// a diff dependency.
func diffOps(a, c []string) []diffOp {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(c)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(c) - 1; j >= 0; j-- {
			if a[i] == c[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(c) {
		switch {
		case a[i] == c[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', c[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(c); j++ {
		ops = append(ops, diffOp{'+', c[j]})
	}
	return ops
}

// diffHunks groups the edit script into rendered hunks, merging changes whose
// context windows touch.
func diffHunks(ops []diffOp) []string {
	var changed []int
	for i, op := range ops {
		if op.mark != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	var out []string
	start := max(changed[0]-diffContext, 0)
	end := min(changed[0]+diffContext+1, len(ops))
	for _, idx := range changed[1:] {
		if idx-diffContext > end {
			out = append(out, renderHunk(ops, start, end))
			start = idx - diffContext
		}
		end = min(idx+diffContext+1, len(ops))
	}
	return append(out, renderHunk(ops, start, end))
}

func renderHunk(ops []diffOp, start, end int) string {
	fromLine, toLine := 1, 1
	for _, op := range ops[:start] {
		if op.mark != '+' {
			fromLine++
		}
		if op.mark != '-' {
			toLine++
		}
	}
	var fromCount, toCount int
	var body strings.Builder
	for _, op := range ops[start:end] {
		if op.mark != '+' {
			fromCount++
		}
		if op.mark != '-' {
			toCount++
		}
		body.WriteByte(op.mark)
		body.WriteString(op.line + "\n")
	}
	// Unified-diff convention: an empty side is numbered by the line before it.
	if fromCount == 0 {
		fromLine--
	}
	if toCount == 0 {
		toLine--
	}
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", fromLine, fromCount, toLine, toCount) + body.String()
}
