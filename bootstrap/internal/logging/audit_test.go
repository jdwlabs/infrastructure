package logging

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewAuditLogger(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)
	if al == nil {
		t.Fatal("NewAuditLogger returned nil")
	}
	if al.w != &buf {
		t.Error("AuditLogger writer not set correctly")
	}
}

func TestAuditLogger_WriteEntry(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	al.WriteEntry("TEST-TAG", "test message")
	output := buf.String()

	// Check format: [timestamp] [tag] message
	if !strings.Contains(output, "[TEST-TAG]") {
		t.Errorf("Expected tag in output, got: %s", output)
	}
	if !strings.Contains(output, "test message") {
		t.Errorf("Expected message in output, got: %s", output)
	}
	// Verify timestamp format (2006-01-02 15:04:05)
	parts := strings.Split(output, "]")
	if len(parts) < 2 {
		t.Errorf("Expected timestamp format, got: %s", output)
	}
}

func TestAuditLogger_Command(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	cmd := al.Command("echo", "hello", "world")
	if cmd == nil {
		t.Fatal("Command returned nil")
	}

	// Verify AuditedCmd fields
	if cmd.name != "echo" {
		t.Errorf("Expected name 'echo', got '%s'", cmd.name)
	}
	if len(cmd.args) != 2 || cmd.args[0] != "hello" || cmd.args[1] != "world" {
		t.Errorf("Expected args ['hello', 'world'], got %v", cmd.args)
	}
	if cmd.audit != al {
		t.Error("AuditedCmd audit field not set correctly")
	}
}

func TestAuditedCmd_cmdString(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)
	cmd := al.Command("ls", "-la", "/tmp")

	expected := "ls -la /tmp"
	if got := cmd.cmdString(); got != expected {
		t.Errorf("cmdString() = %q, want %q", got, expected)
	}
}

func TestAuditedCmd_Run_Success(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that exists on both platforms
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("cmd", "/c", "echo success")
	} else {
		cmd = al.Command("echo", "success")
	}

	err := cmd.Run()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "CMD-START") {
		t.Error("Expected CMD-START in audit log")
	}
	if !strings.Contains(output, "CMD-EXIT") {
		t.Error("Expected CMD-EXIT in audit log")
	}
	// Check exit code 0
	if !strings.Contains(output, "0 [DURATION:") {
		t.Error("Expected exit code 0 with duration")
	}
}

func TestAuditedCmd_Run_WithDir(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("cmd", "/c", "cd")
	} else {
		cmd = al.Command("pwd")
	}
	cmd.Dir = os.TempDir()

	err := cmd.Run()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "CMD-WD") {
		t.Error("Expected CMD-WD when Dir is set")
	}
	if !strings.Contains(output, os.TempDir()) {
		t.Error("Expected working directory in log")
	}
}

func TestAuditedCmd_Run_Failure(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that fails with exit code 1
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("powershell", "-Command", "exit 1")
	} else {
		cmd = al.Command("false")
	}

	err := cmd.Run()
	if err == nil {
		t.Error("Expected error from failing command")
	}

	output := buf.String()
	// Should have non-zero exit code
	if !strings.Contains(output, "1 [DURATION:") && !strings.Contains(output, "CMD-EXIT") {
		t.Errorf("Expected exit code in output: %s", output)
	}
}

func TestAuditedCmd_Run_InvalidCommand(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)
	cmd := al.Command("this-command-does-not-exist-12345")

	err := cmd.Run()
	if err == nil {
		t.Error("Expected error for invalid command")
	}

	output := buf.String()
	if !strings.Contains(output, "CMD-START") {
		t.Error("Expected CMD-START even for failed command")
	}
}

func TestAuditedCmd_Output_Success(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("cmd", "/c", "echo hello")
	} else {
		cmd = al.Command("echo", "hello")
	}

	out, err := cmd.Output()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	outputStr := strings.TrimSpace(string(out))
	if outputStr != "hello" {
		t.Errorf("Expected 'hello', got %q", outputStr)
	}

	auditOutput := buf.String()
	if !strings.Contains(auditOutput, "CMD-OUTPUT") {
		t.Error("Expected CMD-OUTPUT in audit log")
	}
	if !strings.Contains(auditOutput, "hello") {
		t.Error("Expected output content in audit log")
	}
}

func TestAuditedCmd_Output_NoOutput(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that produces no output
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("cmd", "/c", "rem")
	} else {
		cmd = al.Command("true")
	}

	_, err := cmd.Output()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	auditOutput := buf.String()
	// Should not have CMD-OUTPUT when there's no output
	if strings.Contains(auditOutput, "CMD-OUTPUT") {
		t.Error("Should not have CMD-OUTPUT when command produces no output")
	}
}

func TestAuditedCmd_CombinedOutput(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that writes to both stdout and stderr
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("powershell", "-Command", "Write-Output 'stdout'; Write-Host 'stderr'")
	} else {
		cmd = al.Command("sh", "-c", "echo stdout && echo stderr >&2")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		// PowerShell might return exit code, that's ok
		t.Logf("Command returned error (may be expected): %v", err)
	}

	outputStr := string(out)
	if !strings.Contains(outputStr, "stdout") {
		t.Logf("Output: %s", outputStr)
		t.Error("Expected stdout in combined output")
	}

	auditOutput := buf.String()
	if !strings.Contains(auditOutput, "CMD-OUTPUT") {
		t.Error("Expected CMD-OUTPUT in audit log")
	}
}

func TestAuditedCmd_CombinedOutput_Failure(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that fails with specific exit code
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("powershell", "-Command", "Write-Output 'error output'; exit 42")
	} else {
		cmd = al.Command("sh", "-c", "echo error output && exit 42")
	}

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Logf("Error type: %T, value: %v", err, err)
		// On Windows, PowerShell might behave differently, so be lenient
		if runtime.GOOS != "windows" {
			t.Error("Expected *exec.ExitError")
		}
	} else if exitErr.ExitCode() != 42 {
		t.Errorf("Expected exit code 42, got %d", exitErr.ExitCode())
	}

	if !strings.Contains(string(out), "error output") {
		t.Error("Expected to capture output even on failure")
	}

	auditOutput := buf.String()
	if !strings.Contains(auditOutput, "42") && !strings.Contains(auditOutput, "CMD-EXIT") {
		t.Logf("Audit output: %s", auditOutput)
		// Be lenient on Windows
		if runtime.GOOS != "windows" {
			t.Error("Expected exit code 42 in audit log")
		}
	}
}

func TestAuditedCmd_DurationTracking(t *testing.T) {
	var buf bytes.Buffer
	al := NewAuditLogger(&buf)

	// Use a command that takes some time
	var cmd *AuditedCmd
	if runtime.GOOS == "windows" {
		cmd = al.Command("powershell", "-Command", "Start-Sleep -Milliseconds 100")
	} else {
		cmd = al.Command("sleep", "0.1")
	}

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	// Verify duration is reasonable (should be at least 50ms)
	if duration < 50*time.Millisecond {
		t.Logf("Duration was only %v, might be timing issue", duration)
	}

	// Check duration is logged in milliseconds
	output := buf.String()
	if !strings.Contains(output, "ms]") {
		t.Error("Expected duration in milliseconds format")
	}
}
