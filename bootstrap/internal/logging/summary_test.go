package logging

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteSummary_Success(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	data := &SummaryData{
		StartTime:       time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Duration:        5*time.Minute + 30*time.Second,
		Status:          "success",
		ClusterName:     "test-cluster",
		RunDir:          "/logs/2024-01-15/run-20240115_103000",
		ExitError:       nil,
		ControlPlanes:   3,
		Workers:         2,
		AddedNodes:      1,
		RemovedNodes:    0,
		UpdatedConfigs:  2,
		BootstrapNeeded: true,
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	// Verify file was created
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read summary file: %v", err)
	}

	output := string(content)

	// Check all expected content
	expectedStrings := []string{
		"TALOS BOOTSTRAP RUN SUMMARY",
		"Status:       success",
		"Cluster:      test-cluster",
		"Start:        2024-01-15 10:30:00",
		"Duration:     5m30s",
		"Run Dir:      /logs/2024-01-15/run-20240115_103000",
		"Error:        none",
		"Control Planes: 3",
		"Workers:        2",
		"Added:          1",
		"Removed:        0",
		"Updated:        2",
		"Bootstrap:      true",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(output, expected) {
			t.Errorf("Expected output to contain %q", expected)
		}
	}
}

func TestWriteSummary_Failure(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	testErr := &testError{msg: "connection timeout"}
	data := &SummaryData{
		StartTime:       time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Duration:        30 * time.Second,
		Status:          "failed",
		ClusterName:     "failed-cluster",
		RunDir:          "/logs/failed",
		ExitError:       testErr,
		ControlPlanes:   1,
		Workers:         0,
		AddedNodes:      0,
		RemovedNodes:    0,
		UpdatedConfigs:  0,
		BootstrapNeeded: false,
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	content, _ := os.ReadFile(path)
	output := string(content)

	// Check failure-specific content
	if !strings.Contains(output, "Status:       failed") {
		t.Error("Expected failed status")
	}
	if !strings.Contains(output, "Error:        connection timeout") {
		t.Error("Expected error message")
	}
	if strings.Contains(output, "Error:        none") {
		t.Error("Should not have 'none' for error when error exists")
	}
}

func TestWriteSummary_ZeroValues(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	data := &SummaryData{
		StartTime:       time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		Duration:        0,
		Status:          "success",
		ClusterName:     "",
		RunDir:          "",
		ExitError:       nil,
		ControlPlanes:   0,
		Workers:         0,
		AddedNodes:      0,
		RemovedNodes:    0,
		UpdatedConfigs:  0,
		BootstrapNeeded: false,
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	content, _ := os.ReadFile(path)
	output := string(content)

	// Check zero values are formatted correctly
	if !strings.Contains(output, "Duration:     0s") {
		t.Error("Expected 0s duration")
	}
	if !strings.Contains(output, "Control Planes: 0") {
		t.Error("Expected 0 control planes")
	}
	if !strings.Contains(output, "Bootstrap:      false") {
		t.Error("Expected false for bootstrap")
	}
}

func TestWriteSummary_DurationRounding(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	// Test sub-second duration gets rounded
	data := &SummaryData{
		StartTime: time.Now(),
		Duration:  1500 * time.Millisecond, // 1.5 seconds
		Status:    "success",
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	content, _ := os.ReadFile(path)
	output := string(content)

	// Should show 2s (rounded)
	if !strings.Contains(output, "Duration:     2s") {
		t.Errorf("Expected rounded duration '2s', got: %s", output)
	}
}

func TestWriteSummary_InvalidPath(t *testing.T) {
	// Try to write to an invalid path
	path := "/nonexistent/directory/that/does/not/exist/SUMMARY.txt"
	if runtime.GOOS == "windows" {
		// On Windows, use a different invalid path
		path = "\\\\invalid\\path\\SUMMARY.txt"
	}

	data := &SummaryData{
		StartTime: time.Now(),
		Duration:  time.Minute,
		Status:    "success",
	}

	err := WriteSummary(path, data)
	if err == nil {
		t.Error("Expected error for invalid path")
	}
}

func TestWriteSummary_Overwrite(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	// Write first summary
	data1 := &SummaryData{
		StartTime:   time.Now(),
		Duration:    time.Minute,
		Status:      "success",
		ClusterName: "first",
	}
	WriteSummary(path, data1)

	// Write second summary to same path
	data2 := &SummaryData{
		StartTime:   time.Now(),
		Duration:    2 * time.Minute,
		Status:      "failed",
		ClusterName: "second",
	}
	err := WriteSummary(path, data2)
	if err != nil {
		t.Fatalf("WriteSummary (overwrite) failed: %v", err)
	}

	// Verify second write succeeded
	content, _ := os.ReadFile(path)
	output := string(content)

	if !strings.Contains(output, "Cluster:      second") {
		t.Error("Expected second cluster name after overwrite")
	}
	if !strings.Contains(output, "Status:       failed") {
		t.Error("Expected failed status after overwrite")
	}
	if strings.Contains(output, "first") {
		t.Error("Should not contain first cluster name after overwrite")
	}
}

func TestWriteSummary_LongClusterName(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	longName := strings.Repeat("a", 100)
	data := &SummaryData{
		StartTime:   time.Now(),
		Duration:    time.Minute,
		Status:      "success",
		ClusterName: longName,
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), longName) {
		t.Error("Expected long cluster name to be written")
	}
}

func TestWriteSummary_LongErrorMessage(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	longError := &testError{msg: strings.Repeat("error ", 50)}
	data := &SummaryData{
		StartTime: time.Now(),
		Duration:  time.Minute,
		Status:    "failed",
		ExitError: longError,
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), longError.msg) {
		t.Error("Expected long error message to be written")
	}
}

// Test that the file permissions are correct (0644)
// Skip on Windows as it doesn't use Unix permissions
func TestWriteSummary_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping permission test on Windows")
	}

	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "SUMMARY.txt")

	data := &SummaryData{
		StartTime: time.Now(),
		Duration:  time.Minute,
		Status:    "success",
	}

	err := WriteSummary(path, data)
	if err != nil {
		t.Fatalf("WriteSummary failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}

	// Check permissions (0644 = -rw-r--r--)
	expectedMode := os.FileMode(0644)
	if info.Mode().Perm() != expectedMode {
		t.Errorf("Expected file mode %v, got %v", expectedMode, info.Mode().Perm())
	}
}
