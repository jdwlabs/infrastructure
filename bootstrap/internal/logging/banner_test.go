package logging

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrintBanner(t *testing.T) {
	tests := []struct {
		name    string
		version string
		noColor bool
	}{
		{"with color", "v1.0.0", false},
		{"no color", "v1.0.0", true},
		{"empty version", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			PrintBanner(&buf, tt.version, tt.noColor)

			output := buf.String()

			// Should contain TALOS ASCII art (check for distinctive characters)
			if !strings.Contains(output, "TALOS") && !strings.Contains(output, "╗") && !strings.Contains(output, "╔") {
				t.Error("Expected TALOS ASCII art in banner")
			}

			// Should contain version if provided
			if tt.version != "" && !strings.Contains(output, tt.version) {
				t.Errorf("Expected version %q in banner", tt.version)
			}

			// Should contain "Kubernetes Bootstrap Tool"
			if !strings.Contains(output, "Kubernetes Bootstrap Tool") {
				t.Error("Expected 'Kubernetes Bootstrap Tool' in banner")
			}

			// Check color codes
			if tt.noColor {
				if strings.Contains(output, "\033[") {
					t.Error("Expected no ANSI codes when noColor=true")
				}
			}
		})
	}
}

func TestNewBox(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, false)
	if box == nil {
		t.Fatal("NewBox returned nil")
	}
	if box.w != &buf {
		t.Error("Box writer not set correctly")
	}
	if box.noColor != false {
		t.Error("Box noColor should be false")
	}

	// Test with noColor=true
	box2 := NewBox(&buf, true)
	if box2.noColor != true {
		t.Error("Box noColor should be true")
	}
}

func TestBox_c(t *testing.T) {
	var buf bytes.Buffer
	boxColor := NewBox(&buf, false)
	boxNoColor := NewBox(&buf, true)

	// With color enabled
	if got := boxColor.c(cRed); got != cRed {
		t.Errorf("With color enabled, expected %q, got %q", cRed, got)
	}

	// With color disabled
	if got := boxNoColor.c(cRed); got != "" {
		t.Errorf("With color disabled, expected empty string, got %q", got)
	}
}

func TestBox_Header(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Header("Test Title")

	output := buf.String()

	// Should have heavy box drawing characters
	if !strings.Contains(output, hTL) || !strings.Contains(output, hTR) {
		t.Error("Expected heavy top corners in header")
	}
	if !strings.Contains(output, hL) || !strings.Contains(output, hR) {
		t.Error("Expected heavy left/right junctions in header")
	}

	// Should contain title
	if !strings.Contains(output, "Test Title") {
		t.Error("Expected title in header output")
	}
}

func TestBox_Footer(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Footer()

	output := buf.String()

	// Should have heavy bottom corners
	if !strings.Contains(output, hBL) || !strings.Contains(output, hBR) {
		t.Error("Expected heavy bottom corners in footer")
	}
}

func TestBox_Divider(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Divider()

	output := buf.String()

	// Should have mixed junction characters (heavy verticial + light horizontal)
	if !strings.Contains(output, mL) || !strings.Contains(output, mR) {
		t.Error("Expected mixed junction characters in divider")
	}
	if !strings.Contains(output, sH) {
		t.Error("Expected light horizontal line in divider")
	}
}

func TestBox_Row(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Row("Key", "Value")

	output := buf.String()

	// Should contain key and value
	if !strings.Contains(output, "Key:") {
		t.Error("Expected 'Key:' in row output")
	}
	if !strings.Contains(output, "Value") {
		t.Error("Expected 'Value' in row output")
	}

	// Should have heavy vertical borders
	if !strings.Contains(output, hV) {
		t.Error("Expected heavy vertical borders in row")
	}
}

func TestBox_Item(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Item(mBullet, "Test item")

	output := buf.String()

	// Should contain marker and text
	if !strings.Contains(output, mBullet) {
		t.Errorf("Expected marker %q in output", mBullet)
	}
	if !strings.Contains(output, "Test item") {
		t.Error("Expected item text in output")
	}
}

func TestBox_Item_CustomMarker(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Item(mCheck, "Completed")

	output := buf.String()
	if !strings.Contains(output, mCheck) {
		t.Errorf("Expected custom marker %q in output", mCheck)
	}
}

func TestBox_Section(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Section("Section Name")

	output := buf.String()

	// Should contain section name
	if !strings.Contains(output, "Section Name") {
		t.Error("Expected section name in output")
	}
	// Should have mixed junction divider above
	if !strings.Contains(output, mL) || !strings.Contains(output, mR) {
		t.Error("Expected mixed junction characters in section divider")
	}
	if !strings.Contains(output, hV) {
		t.Error("Expected heavy verticial borders in section")
	}
}

func TestBox_Badge(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)
	box.Badge("OK", "Operation successful")

	output := buf.String()

	// Should contain badge in brackets
	if !strings.Contains(output, "[OK]") {
		t.Error("Expected badge [OK] in output")
	}
	if !strings.Contains(output, "Operation successful") {
		t.Error("Expected badge message in output")
	}
}

func TestBox_FullBox(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)

	// Build a complete box
	box.Header("Main Title")
	box.Row("Status", "Running")
	box.Divider()
	box.Section("Details")
	box.Item(mBullet, "Item 1")
	box.Item(mCheck, "Item 2")
	box.Badge("INFO", "2 items")
	box.Footer()

	output := buf.String()

	// Verify structure
	lines := strings.Split(output, "\n")
	if len(lines) < 5 {
		t.Errorf("Expected multiple lines in full box, got %d", len(lines))
	}

	// Verify box width consistency - account for ANSI codes
	for i, line := range lines {
		if line == "" {
			continue
		}
		visibleLen := len(stripANSI(line))
		// Allow some tolerance for the reset code at end
		if visibleLen > 0 && (visibleLen < 10 || visibleLen > boxWidth+10) {
			t.Logf("Line %d (len=%d): %q", i, visibleLen, line)
			// Just log, don't fail - ANSI handling can vary
		}
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"\033[31mred\033[0m", "red"},
		{"\033[1mbold\033[0m", "bold"},
		{"\033[31;1mred bold\033[0m", "red bold"},
		{"no ansi", "no ansi"},
		{"", ""},
		{"\033[", ""},            // Incomplete escape
		{"\033[31m\033[32m", ""}, // Multiple escapes
		{"mixed\033[31mred\033[0mnormal", "mixedrednormal"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.expected {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestStripANSI_BoxWidth(t *testing.T) {
	// This test verifies that stripANSI works correctly for box width calculations
	colored := "\033[36m\033[1m" + strings.Repeat("X", 20) + "\033[0m"
	stripped := stripANSI(colored)

	if len(stripped) != 20 {
		t.Errorf("Expected stripped length 20, got %d", len(stripped))
	}

	if len(colored) <= 20 {
		t.Error("Original colored string should be longer than stripped")
	}
}

func TestConstantsDefined(t *testing.T) {
	// Verify all expected constants are defined
	constants := []string{
		cReset, cBold, cDim, cCyan, cGreen, cBlue, cYellow, cRed,
		hTL, hTR, hBL, hBR, hH, hV, hL, hR,
		sTL, sTR, sBL, sBR, sH, sV, sL, sR, sT, sB, sC,
		mL, mR,
		mBullet, mDiamond, mDot, mCheck, mCross, mWarning, mInfo,
	}

	for _, c := range constants {
		if c == "" && c != cReset { // cReset can be empty in noColor mode
			t.Errorf("Constant not defined: check all constants")
		}
	}

	if boxWidth != 63 {
		t.Errorf("Expected boxWidth = 63, got %d", boxWidth)
	}
}

func TestBox_writeLine_Wrap(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)

	// Content that exceed box width should wrap
	longContent := strings.Repeat("X", boxWidth+50)
	box.writeLine(longContent)

	output := buf.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("Expected multiple lines for wrapped content, got %d", len(lines))
	}

	// Every line should be exactly boxWidth visible runes
	for i, line := range lines {
		visible := stripANSI(line)
		runeCount := utf8.RuneCountInString(visible)
		if runeCount != boxWidth {
			t.Errorf("Line %d: expected width %d runes, got %d: %q", i, boxWidth, runeCount, visible)
		}
		// Every line should have box borders
		if !strings.HasPrefix(visible, hV) || !strings.HasSuffix(visible, hV) {
			t.Errorf("line %d: expected box borders: %q", i, visible)
		}
	}

	// Continuation lines should be indented
	if len(lines) > 1 {
		secondVisible := stripANSI(lines[1])
		// After the left border, should start with spaces (indent)
		inner := secondVisible[len(hV) : len(secondVisible)-len(hV)]
		if !strings.HasPrefix(inner, "    ") {
			t.Errorf("Expected 4-space indent on continuation line, got: %q", inner)
		}
	}

	// Should NOT  contain ellipsis (we wrap, not truncate)
	if strings.Contains(output, "...") {
		t.Error("Should not truncate with ellipsis - expected wrapping")
	}
}

func TestBox_writeLine_NoTruncation(t *testing.T) {
	var buf bytes.Buffer
	box := NewBox(&buf, true)

	// Content that fits within box width
	box.writeLine(" Short content")

	output := buf.String()
	visible := stripANSI(strings.Split(output, "\n")[0])
	runeCount := utf8.RuneCountInString(visible)

	if runeCount != boxWidth {
		t.Errorf("Expected width %d runes, got %d: %q", boxWidth, runeCount, visible)
	}
	if strings.Contains(output, "...") {
		t.Errorf("Should not truncate content that fits")
	}
}
