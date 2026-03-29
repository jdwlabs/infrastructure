package logging

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const boxWidth = 63

// ANSI colors
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cCyan   = "\033[36m"
	cGreen  = "\033[32m"
	cBlue   = "\033[34m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
)

// Heavy box-drawing (outer frame)
const (
	hTL = "┏" // U+250F
	hTR = "┓" // U+2513
	hBL = "┗" // U+2517
	hBR = "┛" // U+251B
	hH  = "━" // U+2501
	hV  = "┃" // U+2503
	hL  = "┣" // U+2523
	hR  = "┫" // U+252B
)

// Light box-drawing (internal dividers)
const (
	sTL = "┌" // U+250C
	sTR = "┐" // U+2510
	sBL = "└" // U+2514
	sBR = "┘" // U+2518
	sH  = "─" // U+2500
	sV  = "│" // U+2502
	sL  = "├" // U+251C
	sR  = "┤" // U+2524
	sT  = "┬" // U+252C
	sB  = "┴" // U+2534
	sC  = "┼" // U+253C
)

// Mixed junctions (heavy vertical + light horization)
const (
	mL = "┠" // U+2520 - heavy vertical, light right
	mR = "┨" // U+2528 - heavy vertical, light left
)

// Markers
const (
	mBullet  = "•" // U+2022
	mDiamond = "◆" // U+25C6
	mDot     = "·" // U+00B7
	mCheck   = "✓" // U+2713
	mCross   = "✗" // U+2717
	mWarning = "⚠" // U+26A0
	mInfo    = "ℹ" // U+2139
)

// talosASCIIArt is the filled block art for TALOS
const talosASCIIArt = `████████╗ █████╗ ██╗      ██████╗ ███████╗
╚══██╔══╝██╔══██╗██║     ██╔═══██╗██╔════╝
   ██║   ███████║██║     ██║   ██║███████╗
   ██║   ██╔══██║██║     ██║   ██║╚════██║
   ██║   ██║  ██║███████╗╚██████╔╝███████║
   ╚═╝   ╚═╝  ╚═╝╚══════╝ ╚═════╝ ╚══════╝`

// PrintBanner writes the TALOS banner to w.
func PrintBanner(w io.Writer, version string, noColor bool) {
	cc, cb, cd, cr := cCyan, cBold, cDim, cReset
	if noColor {
		cc, cb, cd, cr = "", "", "", ""
	}
	fmt.Fprintf(w, "%s%s%s\n%s%s ━━━ Kubernetes Bootstrap Tool %s ━━━%s\n",
		cc, cb, talosASCIIArt, cr, cd, version, cr)
}

// Box provides box-drawing UI output.
type Box struct {
	w       io.Writer
	noColor bool
}

// NewBox creates a Box that writes to w.
func NewBox(w io.Writer, noColor bool) *Box {
	return &Box{w: w, noColor: noColor}
}

func (b *Box) c(code string) string {
	if b.noColor {
		return ""
	}
	return code
}

// writeLine writes content with heavy vertical borders and padding.
// If the visible context exceeds the box inner width, the text wraps onto
// continuation lines so the right border stays aligned. ANSI colors active
// at the break point are carried into continuation lines.
func (b *Box) writeLine(content string) {
	visible := stripANSI(content)
	maxInner := boxWidth - 2
	visLen := utf8.RuneCountInString(visible)

	if visLen <= maxInner {
		padding := maxInner - visLen
		fmt.Fprintf(b.w, "%s%s%s%s%s%s%s%s\n",
			b.c(cDim), hV, b.c(cReset),
			content,
			strings.Repeat(" ", padding),
			b.c(cDim), hV, b.c(cReset))
		return
	}

	// Break 1 char early so wrapped lines have a space before the right border
	wrapAt := maxInner - 1

	// First line: render with original ANSI content, trimmed to wrapAt visible chars
	first := truncateVisible(content, wrapAt)
	padding := maxInner - wrapAt
	fmt.Fprintf(b.w, "%s%s%s%s%s%s%s%s\n",
		b.c(cDim), hV, b.c(cReset),
		first,
		strings.Repeat(" ", padding),
		b.c(cDim), hV, b.c(cReset))

	// Determine the ANSI color active at the break point so continuation
	// lines can carry forward the same color.
	activeColor := ansiStateAt(content, wrapAt)

	// Wrap remaining visible text onto continuation lines (indent 4 spaces)
	runes := []rune(visible)
	const wrapIndent = 4
	wrapWidth := wrapAt - wrapIndent
	pos := wrapAt
	for pos < len(runes) {
		end := pos + wrapWidth
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[pos:end])
		line := strings.Repeat(" ", wrapIndent) + b.c(activeColor) + chunk + b.c(cReset)
		padding := maxInner - utf8.RuneCountInString(strings.Repeat(" ", wrapIndent)+chunk)
		fmt.Fprintf(b.w, "%s%s%s%s%s%s%s%s\n",
			b.c(cDim), hV, b.c(cReset),
			line,
			strings.Repeat(" ", padding),
			b.c(cDim), hV, b.c(cReset))
		pos = end
	}
}

// truncateVisible returns a prefix of s whose visible (non-ANSI) length is
// exactly n runes. Any open ANSI escape at the cut point is completed, and a
// trailing reset is appended so colors don't bleed.
func truncateVisible(s string, n int) string {
	var out strings.Builder
	visible := 0
	inEscape := false
	hadEscape := false
	for _, r := range s {
		if visible >= n && !inEscape {
			break
		}
		out.WriteRune(r)
		if r == '\033' {
			inEscape = true
			hadEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		visible++
	}
	if hadEscape {
		out.WriteString(cReset)
	}
	return out.String()
}

// ansiStateAt returns the last ANSI escape code active at the given visible
// character position n. If no color is active (or it was reset), returns "".
func ansiStateAt(s string, n int) string {
	var lastCode string
	var cur strings.Builder
	visible := 0
	inEscape := false
	for _, r := range s {
		if visible >= n && !inEscape {
			break
		}
		if r == '\033' {
			inEscape = true
			cur.Reset()
			cur.WriteRune(r)
			continue
		}
		if inEscape {
			cur.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
				code := cur.String()
				if code == cReset {
					lastCode = ""
				} else {
					lastCode = code
				}
			}
			continue
		}
		visible++
	}
	return lastCode
}

// Header writes the heavy top border and title.
func (b *Box) Header(title string) {
	top := strings.Repeat(hH, boxWidth-2)
	fmt.Fprintf(b.w, "%s%s%s%s%s\n",
		b.c(cDim), hTL, top, hTR, b.c(cReset))

	b.writeLine(fmt.Sprintf(" %s%s%s%s", b.c(cCyan), b.c(cBold), title, b.c(cReset)))

	sep := strings.Repeat(hH, boxWidth-2)
	fmt.Fprintf(b.w, "%s%s%s%s%s\n",
		b.c(cDim), hL, sep, hR, b.c(cReset))
}

// Footer writes the heavy bottom border.
func (b *Box) Footer() {
	bottom := strings.Repeat(hH, boxWidth-2)
	fmt.Fprintf(b.w, "%s%s%s%s%s\n",
		b.c(cDim), hBL, bottom, hBR, b.c(cReset))
}

// Divider writes a light horizontal separator with proper heavy-to-light junctions.
func (b *Box) Divider() {
	inner := strings.Repeat(sH, boxWidth-2)
	fmt.Fprintf(b.w, "%s%s%s%s%s\n",
		b.c(cDim), mL, inner, mR, b.c(cReset))
}

// Label writes a bold label line without a preceding divider.
func (b *Box) Label(label string) {
	b.writeLine(fmt.Sprintf(" %s%s%s", b.c(cBold), label, b.c(cReset)))
}

// Row writes a key: value pair.
func (b *Box) Row(key, value string) {
	b.writeLine(fmt.Sprintf("  %s: %s%s%s", key, b.c(cCyan), value, b.c(cReset)))
}

// Item writes a bulleted item with color based on the marker.
func (b *Box) Item(marker, text string) {
	var color string
	switch marker {
	case "+":
		color = cGreen
	case "-":
		color = cRed
	case "~":
		color = cYellow
	case "$":
		color = cDim
	case mCheck:
		color = cGreen
	case mCross:
		color = cRed
	case mWarning:
		color = cYellow
	}
	if color != "" {
		b.writeLine(fmt.Sprintf("  %s%s%s %s", b.c(color), marker, b.c(cReset), text))
	} else {
		b.writeLine(fmt.Sprintf("  %s %s", marker, text))
	}
}

// Section writes a section header with a light divider line above it.
func (b *Box) Section(label string) {
	b.Divider()
	b.writeLine(fmt.Sprintf(" %s%s%s", b.c(cBold), label, b.c(cReset)))
}

// Badge writes a colored [BADGE] message. Color is chosen by badge name:
// OK/SUCCESS -> green, BOOTSTRAP/INFO/WARN -> yellow, ERROR/FAIL -> red.
func (b *Box) Badge(badge, msg string) {
	var color string
	switch badge {
	case "OK", "SUCCESS", "PASS":
		color = cGreen
	case "ERROR", "FAIL":
		color = cRed
	default:
		color = cYellow
	}
	b.writeLine(fmt.Sprintf("  %s[%s]%s %s", b.c(color), badge, b.c(cReset), msg))
}

// stripANSI removes ANSI escape sequences.
func stripANSI(s string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
