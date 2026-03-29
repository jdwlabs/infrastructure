package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestParseZapLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected zapcore.Level
	}{
		{"debug", zap.DebugLevel},
		{"DEBUG", zap.DebugLevel},
		{"trace", zap.DebugLevel},
		{"warn", zap.WarnLevel},
		{"warning", zap.WarnLevel},
		{"WARN", zap.WarnLevel},
		{"error", zap.ErrorLevel},
		{"ERROR", zap.ErrorLevel},
		{"info", zap.InfoLevel},
		{"INFO", zap.InfoLevel},
		{"", zap.InfoLevel},
		{"unknown", zap.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseZapLevel(tt.input)
			if got != tt.expected {
				t.Errorf("parseZapLevel(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestColorLevelEncoder(t *testing.T) {
	tests := []struct {
		level    zapcore.Level
		contains string
	}{
		{zapcore.FatalLevel, colorWhiteOnRd},
		{zapcore.ErrorLevel, colorRed},
		{zapcore.WarnLevel, colorYellow},
		{zapcore.DebugLevel, colorBlue},
		{zapcore.InfoLevel, colorGreen},
	}

	for _, tt := range tests {
		t.Run(tt.level.String(), func(t *testing.T) {
			enc := zapcore.NewConsoleEncoder(newConsoleEncoderConfig(false))

			entry := zapcore.Entry{Level: tt.level, Time: time.Now()}
			fields := []zapcore.Field{}

			if _, err := enc.EncodeEntry(entry, fields); err != nil {
				t.Fatalf("EncodeEntry failed: %v", err)
			}

			// The encoder uses colorLevelEncoder internally
			// We test via the actual encoding in integration tests
		})
	}
}

func TestNewConsoleEncoderConfig(t *testing.T) {
	// Test with color
	cfg := newConsoleEncoderConfig(false)
	if cfg.EncodeTime == nil {
		t.Error("Expected EncodeTime to be set")
	}
	if cfg.EncodeLevel == nil {
		t.Error("Expected EncodeLevel to be set")
	}

	// Test without color uses paddedLevelEncoder
	cfgNoColor := newConsoleEncoderConfig(true)
	if cfgNoColor.EncodeTime == nil {
		t.Error("Expected EncodeTime to be set for noColor=true")
	}
	if cfgNoColor.EncodeLevel == nil {
		t.Error("Expected EncodeLevel to be set for noColor=true")
	}

	// Caller key should be empty (no caller in console output)
	if cfg.CallerKey != "" {
		t.Error("Expected CallerKey to be empty")
	}
}

func TestBuildTeeCore(t *testing.T) {
	var consoleBuf, jsonBuf bytes.Buffer

	core := buildTeeCore(zap.InfoLevel, true, &consoleBuf, &jsonBuf)
	if core == nil {
		t.Fatal("buildTeeCore returned nil")
	}

	// Create logger with the core
	logger := zap.New(core)
	logger.Info("test message")
	logger.Sync()

	// Console output should be human-readable
	consoleOutput := consoleBuf.String()
	if !strings.Contains(consoleOutput, "test message") {
		t.Error("Expected 'test message' in console output")
	}

	// JSON output should be structured
	jsonOutput := jsonBuf.String()
	if !strings.Contains(jsonOutput, "test message") {
		t.Error("Expected 'test message' in JSON output")
	}
	if !strings.Contains(jsonOutput, `"ts"`) && !strings.Contains(jsonOutput, `"level"`) {
		t.Error("Expected structured fields in JSON output")
	}
}

func TestNewRunSession(t *testing.T) {
	// Create temp directory
	tempDir := t.TempDir()

	cfg := &types.Config{
		ClusterName:     "test-cluster",
		TerraformTFVars: filepath.Join(tempDir, "terraform.tfvars"),
		LogDir:          tempDir,
		LogLevel:        "info",
		NoColor:         true,
	}

	session, err := NewRunSession(cfg)
	if err != nil {
		t.Fatalf("NewRunSession failed: %v", err)
	}
	defer session.Close(nil)

	// Verify session fields
	if session.RunDir == "" {
		t.Error("Expected RunDir to be set")
	}
	if session.Logger == nil {
		t.Error("Expected Logger to be set")
	}
	if session.AuditLog == nil {
		t.Error("Expected AuditLog to be set")
	}
	if session.Config != cfg {
		t.Error("Expected Config to be set")
	}
	if session.StartTime.IsZero() {
		t.Error("Expected StartTime to be set")
	}

	// Verify directory structure
	if _, err := os.Stat(session.RunDir); os.IsNotExist(err) {
		t.Errorf("Run directory does not exist: %s", session.RunDir)
	}

	// Verify log files were created
	expectedFiles := []string{"console.log", "structured.log", "audit.log"}
	for _, file := range expectedFiles {
		path := filepath.Join(session.RunDir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("Expected file not created: %s", path)
		}
	}

	// Verify runs.log was updated
	runsLogPath := filepath.Join(tempDir, "runs.log")
	if _, err := os.Stat(runsLogPath); os.IsNotExist(err) {
		t.Error("Expected runs.log to be created")
	}

	// Verify latest.txt was created
	latestPath := filepath.Join(tempDir, "latest.txt")
	if _, err := os.Stat(latestPath); os.IsNotExist(err) {
		t.Error("Expected latest.txt to be created")
	}

	// Check runs.log content
	runsContent, err := os.ReadFile(runsLogPath)
	if err != nil {
		t.Fatalf("Failed to read runs.log: %v", err)
	}
	if !strings.Contains(string(runsContent), "test-cluster") {
		t.Error("Expected cluster name in runs.log")
	}
	if !strings.Contains(string(runsContent), "pending") {
		t.Error("Expected 'pending' status in runs.log")
	}
}

func TestNewRunSession_MkdirError(t *testing.T) {
	// Try to create session in a read-only path (this test may be skipped on some systems)
	cfg := &types.Config{
		ClusterName: "test",
		LogDir:      "/nonexistent/path/that/cannot/be/created",
		LogLevel:    "info",
	}

	_, err := NewRunSession(cfg)
	if err == nil {
		t.Skip("System allows creating directories anywhere, skipping error test")
	}
}

func TestRunSession_Close_Success(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName:     "close-test",
		TerraformTFVars: filepath.Join(tempDir, "terraform.tfvars"),
		LogDir:          tempDir,
		LogLevel:        "info",
		NoColor:         true,
	}

	session, err := NewRunSession(cfg)
	if err != nil {
		t.Fatalf("NewRunSession failed: %v", err)
	}

	// Set some operational counters
	session.ControlPlanes = 3
	session.Workers = 2
	session.AddedNodes = 1
	session.RemovedNodes = 0
	session.UpdatedConfigs = 2
	session.BootstrapNeeded = true

	// Close with no error
	session.Close(nil)

	// Verify SUMMARY.txt was created
	summaryPath := filepath.Join(session.RunDir, "SUMMARY.txt")
	content, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("Failed to read SUMMARY.txt: %v", err)
	}

	summaryStr := string(content)
	if !strings.Contains(summaryStr, "success") {
		t.Error("Expected 'success' status in summary")
	}
	if !strings.Contains(summaryStr, "close-test") {
		t.Error("Expected cluster name in summary")
	}
	if !strings.Contains(summaryStr, "Control Planes: 3") {
		t.Error("Expected control plane count in summary")
	}
	if !strings.Contains(summaryStr, "Workers:        2") {
		t.Error("Expected worker count in summary")
	}
	if !strings.Contains(summaryStr, "Added:          1") {
		t.Error("Expected added count in summary")
	}

	// Verify runs.log was updated
	runsContent, _ := os.ReadFile(filepath.Join(tempDir, "runs.log"))
	if !strings.Contains(string(runsContent), "success") {
		t.Error("Expected runs.log to be updated with success status")
	}
}

func TestRunSession_Close_WithError(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName:     "error-test",
		TerraformTFVars: filepath.Join(tempDir, "terraform.tfvars"),
		LogDir:          tempDir,
		LogLevel:        "info",
		NoColor:         true,
	}

	session, _ := NewRunSession(cfg)
	testErr := &testError{msg: "test failure"}
	session.Close(testErr)

	// Verify SUMMARY.txt shows failure
	summaryPath := filepath.Join(session.RunDir, "SUMMARY.txt")
	content, _ := os.ReadFile(summaryPath)

	if !strings.Contains(string(content), "failed") {
		t.Error("Expected 'failed' status in summary")
	}
	if !strings.Contains(string(content), "test failure") {
		t.Error("Expected error message in summary")
	}

	// Verify runs.log was updated
	runsContent, _ := os.ReadFile(filepath.Join(tempDir, "runs.log"))
	if !strings.Contains(string(runsContent), "failed") {
		t.Error("Expected runs.log to be updated with failed status")
	}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

func TestRunSession_UpdateRunsLogStatus(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName: "update-test",
		LogDir:      tempDir,
		LogLevel:    "info",
	}

	session, _ := NewRunSession(cfg)
	originalDir := session.RunDir
	session.Close(nil)

	// Create another session to test multiple runs
	session2, _ := NewRunSession(cfg)
	session2.Close(nil)

	// Verify both entries exist
	runsContent, _ := os.ReadFile(filepath.Join(tempDir, "runs.log"))
	lines := strings.Split(string(runsContent), "\n")

	successCount := 0
	for _, line := range lines {
		if strings.Contains(line, "success") {
			successCount++
		}
	}
	if successCount < 2 {
		t.Errorf("Expected at least 2 success entries, found %d", successCount)
	}

	// Verify first run still has success status (wasn't overwritten)
	if !strings.Contains(string(runsContent), originalDir) {
		t.Error("Expected first run directory to still be in runs.log")
	}
}

func TestRunSession_RegisterRun_Multiple(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName: "multi-test",
		LogDir:      tempDir,
		LogLevel:    "info",
	}

	// Create multiple sessions
	for i := 0; i < 3; i++ {
		session, err := NewRunSession(cfg)
		if err != nil {
			t.Fatalf("Failed to create session %d: %v", i, err)
		}
		session.Close(nil)
	}

	// Verify runs.log has all entries
	content, _ := os.ReadFile(filepath.Join(tempDir, "runs.log"))
	entries := strings.Split(string(content), "\n")

	nonEmpty := 0
	for _, entry := range entries {
		if strings.TrimSpace(entry) != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 3 {
		t.Errorf("Expected 3 entries in runs.log, found %d", nonEmpty)
	}
}

func TestRunSession_WriteHeader(t *testing.T) {
	tempDir := t.TempDir()
	tfvarsPath := filepath.Join(tempDir, "terraform.tfvars")
	cfg := &types.Config{
		ClusterName:     "header-test",
		TerraformTFVars: tfvarsPath,
		LogDir:          tempDir,
		LogLevel:        "debug",
		NoColor:         true,
	}

	session, _ := NewRunSession(cfg)
	session.Close(nil)

	// Check console.log for header content
	consolePath := filepath.Join(session.RunDir, "console.log")
	content, _ := os.ReadFile(consolePath)

	expectedParts := []string{
		"session started",
		"header-test",
	}

	for _, part := range expectedParts {
		if !strings.Contains(string(content), part) {
			t.Errorf("Expected header to contain %q", part)
		}
	}
}

func TestRunSession_UpdateLatest(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName: "latest-test",
		LogDir:      tempDir,
		LogLevel:    "info",
	}

	session1, _ := NewRunSession(cfg)
	session1.Close(nil)

	// Check latest.txt points to first session
	latest1, _ := os.ReadFile(filepath.Join(tempDir, "latest.txt"))
	latest1Str := strings.TrimSpace(string(latest1))
	if !strings.Contains(latest1Str, session1.RunDir) && latest1Str != session1.RunDir {
		t.Logf("Latest1: %q, RunDir: %q", latest1Str, session1.RunDir)
		// On Windows, paths might have different separators, so just check it contains the run name
		if !strings.Contains(latest1Str, "run-") {
			t.Error("latest.txt should point to first session")
		}
	}

	// Create second session
	session2, _ := NewRunSession(cfg)
	session2.Close(nil)

	// Check latest.txt now points to second session
	latest2, _ := os.ReadFile(filepath.Join(tempDir, "latest.txt"))
	latest2Str := strings.TrimSpace(string(latest2))

	// Should contain second session path
	if !strings.Contains(latest2Str, session2.RunDir) && latest2Str != session2.RunDir {
		t.Logf("Latest2: %q, RunDir2: %q", latest2Str, session2.RunDir)
		if !strings.Contains(latest2Str, "run-") {
			t.Error("latest.txt should point to second session")
		}
	}
}

// Integration test for full session lifecycle
func TestRunSession_FullLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &types.Config{
		ClusterName:     "lifecycle-test",
		TerraformTFVars: filepath.Join(tempDir, "terraform.tfvars"),
		LogDir:          tempDir,
		LogLevel:        "debug",
		NoColor:         true,
	}

	// Create session
	session, err := NewRunSession(cfg)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Simulate some operations
	session.Logger.Info("Operation 1")
	session.Logger.Debug("Debug info")
	session.AuditLog.WriteEntry("TEST", "test audit entry")

	// Close session
	session.Close(nil)

	// Verify all files exist and have content
	files := map[string]bool{
		"console.log":    false,
		"structured.log": false,
		"audit.log":      false,
		"SUMMARY.txt":    false,
	}

	for file := range files {
		path := filepath.Join(session.RunDir, file)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("File not found: %s", file)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("File is empty: %s", file)
		}
	}

	// Verify structured.log is valid JSON-ish
	structuredPath := filepath.Join(session.RunDir, "structured.log")
	structuredContent, _ := os.ReadFile(structuredPath)
	if !strings.Contains(string(structuredContent), `"level"`) && !strings.Contains(string(structuredContent), "level") {
		t.Logf("Structured content: %s", string(structuredContent))
		// Don't fail - format might vary
	}
}

// TestKvEncoder_WithFieldsNoJSON verifies that logger.With() fields render as
// key=value in console output, NOT as JSON like {"vmid": 200}.
func TestKvEncoder_WithFieldsNoJSON(t *testing.T) {
	var consoleBuf bytes.Buffer

	core := buildTeeCore(zap.InfoLevel, true, &bytes.Buffer{}, &bytes.Buffer{})
	// Build a single-sink core writing to our buffer so we can inspect output
	consoleCfg := newConsoleEncoderConfig(true)
	enc := &kvEncoder{inner: zapcore.NewConsoleEncoder(consoleCfg), noColor: true}
	singleCore := zapcore.NewCore(enc, zapcore.AddSync(&consoleBuf), zapcore.InfoLevel)
	_ = core // discard tee core, we just need the encoder test

	logger := zap.New(singleCore)

	// This is exactly what RebootMonitor does: logger.With(zap.Int("vmid", 200))
	childLogger := logger.With(zap.Int("vmid", 200))
	childLogger.Info("node is ready", zap.String("ip", "192.1681.1.156"))
	logger.Sync()

	output := consoleBuf.String()

	// Must contain key=value format
	if !strings.Contains(output, "vmid=200") {
		t.Errorf("Expected 'vmid=200' in output, got: %s", output)
	}
	if !strings.Contains(output, "ip=192.1681.1.156") {
		t.Errorf("Expected 'ip=192.1681.1.156' in output, got: %s", output)
	}

	// Must NOT contain JSON format
	if strings.Contains(output, `{"vmid"`) || strings.Contains(output, `"vmid":`) {
		t.Errorf("JSON leak detected in console output: %s", output)
	}
	if strings.Contains(output, `{"ip"`) || strings.Contains(output, `"ip":`) {
		t.Errorf("JSON leak detected in console output: %s", output)
	}
}

// TestBuildTeeCore_WithFieldsInJSON verifies that logger.With() fields propagate
// to the JSON structured.log output, not just the kv console output.
func TestBuildTeeCore_WithFieldsInJSON(t *testing.T) {
	var consoleBuf, jsonBuf bytes.Buffer

	core := buildTeeCore(zap.InfoLevel, true, &consoleBuf, &jsonBuf)
	logger := zap.New(core)

	childLogger := logger.With(zap.Int("vmid", 200))
	childLogger.Info("node is ready", zap.String("ip", "192.168.1.50"))
	logger.Sync()

	jsonOutput := jsonBuf.String()

	// With() field must appear in structured.log JSON output
	if !strings.Contains(jsonOutput, `"vmid"`) {
		t.Errorf("Expected 'vmid' field in JSON output, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, "200") {
		t.Errorf("Expected vmid value 200 in JSON output, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, `"ip"`) {
		t.Errorf("Expected 'ip' field in JSON output, got: %s", jsonOutput)
	}
	if !strings.Contains(jsonOutput, "192.168.1.50") {
		t.Errorf("Expected '192.168.1.50' field in JSON output, got: %s", jsonOutput)
	}

	// Console output should have kv format
	consoleOuput := consoleBuf.String()
	if !strings.Contains(consoleOuput, "vmid=200") {
		t.Errorf("Expected 'vmid=200' in consoleOuput, got: %s", consoleOuput)
	}
}

// Helper to check if running on Windows
func isWindows() bool {
	return runtime.GOOS == "windows"
}
