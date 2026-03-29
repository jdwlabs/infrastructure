package terraform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// mockExecCommand is a helper to mock exec.CommandContext
// Usage: execCommandContext = mockExecCommand(expectedOutput, expectedError)
func mockExecCommand(output string, exitErr error) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, command string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestHelperProcess", "--", command}
		cs = append(cs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = []string{
			"GO_WANT_HELPER_PROCESS=1",
			"GO_HELPER_OUTPUT=" + output,
			"GO_HELPER_ERROR=" + fmt.Sprintf("%v", exitErr),
		}
		return cmd
	}
}

// TestHelperProcess is a helper process for mocking exec commands
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	output := os.Getenv("GO_HELPER_OUTPUT")
	errStr := os.Getenv("GO_HELPER_ERROR")

	if output != "" {
		fmt.Fprint(os.Stdout, output)
	}

	if errStr != "" && errStr != "<nil>" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestLookPath(t *testing.T) {
	// This test may pass or fail depending on whether terraform is installed
	// We just verify it doesn't panic and returns appropriate values
	path, err := LookPath()
	if err != nil {
		// Terraform not in PATH - this is acceptable
		if !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("LookPath() unexpected error = %v", err)
		}
	} else {
		// Terraform found - verify path is not empty
		if path == "" {
			t.Error("LookPath() returned empty path with nil error")
		}
	}
}

func TestNewRunner(t *testing.T) {
	logger := zaptest.NewLogger(t)
	dir := "/tmp/terraform"

	runner := NewRunner(dir, logger)

	if runner == nil {
		t.Fatal("NewRunner() returned nil")
	}
	if runner.dir != dir {
		t.Errorf("NewRunner() dir = %v, want %v", runner.dir, dir)
	}
	if runner.logger != logger {
		t.Error("NewRunner() logger not set correctly")
	}
}

func TestRunner_Init(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		exitErr     error
		wantErr     bool
		errContains string
	}{
		{
			name:    "successful init",
			output:  "Initializing modules...\nInitializing provider plugins...",
			exitErr: nil,
			wantErr: false,
		},
		{
			name:        "init fails",
			output:      "Error: Invalid provider configuration",
			exitErr:     errors.New("exit status 1"),
			wantErr:     true,
			errContains: "terraform init",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore original execCommandContext
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			err := runner.Init(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Init() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Init() error = %v, should contain %v", err, tt.errContains)
				}
			}
		})
	}
}

func TestRunner_Fmt(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		exitErr error
		wantErr bool
	}{
		{
			name:    "successful fmt",
			output:  "",
			exitErr: nil,
			wantErr: false,
		},
		{
			name:    "fmt fails",
			output:  "Error: Invalid block definition",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			err := runner.Fmt(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Fmt() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunner_Validate(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		exitErr error
		wantErr bool
	}{
		{
			name:    "successful validation",
			output:  "Success! The configuration is valid.",
			exitErr: nil,
			wantErr: false,
		},
		{
			name:    "validation fails",
			output:  "Error: Reference to undeclared resource",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			err := runner.Validate(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunner_Plan(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		exitCode    int
		wantChanges bool
		wantErr     bool
		errContains string
	}{
		{
			name:        "no changes",
			output:      "No changes. Your infrastructure matches the configuration.",
			exitCode:    0,
			wantChanges: false,
			wantErr:     false,
		},
		{
			name:        "changes present (exit code 2)",
			output:      "Plan: 1 to add, 0 to change, 0 to destroy.",
			exitCode:    2,
			wantChanges: true,
			wantErr:     false,
		},
		{
			name:        "plan fails",
			output:      "Error: Invalid resource type",
			exitCode:    1,
			wantChanges: false,
			wantErr:     true,
			errContains: "terraform plan",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			// Custom mock that handles exit codes properly
			execCommandContext = func(ctx context.Context, command string, args ...string) *exec.Cmd {
				cs := []string{"-test.run=TestHelperProcessPlan", "--", command}
				cs = append(cs, args...)
				cmd := exec.CommandContext(ctx, os.Args[0], cs...)
				cmd.Env = []string{
					"GO_WANT_HELPER_PROCESS=1",
					"GO_HELPER_OUTPUT=" + tt.output,
					"GO_HELPER_EXIT_CODE=" + fmt.Sprintf("%d", tt.exitCode),
				}
				return cmd
			}

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			gotChanges, err := runner.Plan(context.Background(), "/tmp/plan.out")
			if (err != nil) != tt.wantErr {
				t.Errorf("Plan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotChanges != tt.wantChanges {
				t.Errorf("Plan() gotChanges = %v, want %v", gotChanges, tt.wantChanges)
			}
			if tt.wantErr && tt.errContains != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Plan() error = %v, should contain %v", err, tt.errContains)
				}
			}
		})
	}
}

// TestHelperProcessPlan handles exit codes for Plan tests
func TestHelperProcessPlan(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	output := os.Getenv("GO_HELPER_OUTPUT")
	exitCodeStr := os.Getenv("GO_HELPER_EXIT_CODE")

	if output != "" {
		fmt.Fprint(os.Stdout, output)
	}

	var exitCode int
	fmt.Sscanf(exitCodeStr, "%d", &exitCode)
	os.Exit(exitCode)
}

func TestRunner_DestroyPlan(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		exitErr error
		wantErr bool
	}{
		{
			name:    "successful destroy plan",
			output:  "Plan: 0 to add, 0 to change, 5 to destroy.",
			exitErr: nil,
			wantErr: false,
		},
		{
			name:    "destroy plan fails",
			output:  "Error: Failed to load state",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			err := runner.DestroyPlan(context.Background(), "/tmp/destroy.plan")
			if (err != nil) != tt.wantErr {
				t.Errorf("DestroyPlan() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunner_ShowJSON(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		exitErr     error
		wantErr     bool
		errContains string
		wantPlan    *PlanOutput
	}{
		{
			name:    "valid plan output",
			exitErr: nil,
			output: `{
				"resource_changes": [
					{
						"address": "proxmox_vm_qemu.test",
						"type": "proxmox_vm_qemu",
						"name": "test",
						"change": {
							"actions": ["create"]
						}
					}
				]
			}`,
			wantErr: false,
			wantPlan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{
						Address: "proxmox_vm_qemu.test",
						Type:    "proxmox_vm_qemu",
						Name:    "test",
						Change:  Change{Actions: []string{"create"}},
					},
				},
			},
		},
		{
			name:        "command fails",
			output:      "",
			exitErr:     errors.New("exit status 1"),
			wantErr:     true,
			errContains: "terraform show",
		},
		{
			name:        "invalid json",
			output:      "not valid json",
			exitErr:     nil,
			wantErr:     true,
			errContains: "parse terraform plan JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			plan, err := runner.ShowJSON(context.Background(), "/tmp/plan.out")
			if (err != nil) != tt.wantErr {
				t.Errorf("ShowJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && tt.wantPlan != nil {
				if len(plan.ResourceChanges) != len(tt.wantPlan.ResourceChanges) {
					t.Errorf("ShowJSON() resource changes = %d, want %d",
						len(plan.ResourceChanges), len(tt.wantPlan.ResourceChanges))
				}
			}
			if tt.wantErr && tt.errContains != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ShowJSON() error = %v, should contain %v", err, tt.errContains)
				}
			}
		})
	}
}

func TestRunner_ShowStateJSON(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		exitErr error
		wantErr bool
	}{
		{
			name:    "valid state output",
			exitErr: nil,
			output: `{
				"values": {
					"root_module": {
						"resources": []
					}
				}
			}`,
			wantErr: false,
		},
		{
			name:    "command fails",
			output:  "",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			state, err := runner.ShowStateJSON(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("ShowStateJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && state == nil {
				t.Error("ShowStateJSON() returned nil state without error")
			}
		})
	}
}

func TestRunner_StateList(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		exitErr       error
		wantErr       bool
		wantResources []string
	}{
		{
			name:          "resources found",
			output:        "proxmox_vm_qemu.vm1\nproxmox_vm_qemu.vm2\n",
			exitErr:       nil,
			wantErr:       false,
			wantResources: []string{"proxmox_vm_qemu.vm1", "proxmox_vm_qemu.vm2"},
		},
		{
			name:          "empty state",
			output:        "",
			exitErr:       nil,
			wantErr:       false,
			wantResources: []string{},
		},
		{
			name:    "command fails",
			output:  "",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			resources, err := runner.StateList(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("StateList() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(resources) != len(tt.wantResources) {
					t.Errorf("StateList() resources = %v, want %v", resources, tt.wantResources)
				}
				for i, r := range resources {
					if r != tt.wantResources[i] {
						t.Errorf("StateList() resource[%d] = %v, want %v", i, r, tt.wantResources[i])
					}
				}
			}
		})
	}
}

func TestRunner_Version(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		exitErr     error
		wantErr     bool
		wantVersion string
	}{
		{
			name:        "valid version",
			output:      `{"terraform_version":"1.5.0"}`,
			exitErr:     nil,
			wantErr:     false,
			wantVersion: "1.5.0",
		},
		{
			name:    "command fails",
			output:  "",
			exitErr: errors.New("exit status 1"),
			wantErr: true,
		},
		{
			name:    "invalid json",
			output:  "not json",
			exitErr: nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			execCommandContext = mockExecCommand(tt.output, tt.exitErr)

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			version, err := runner.Version(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Version() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && version != tt.wantVersion {
				t.Errorf("Version() = %v, want %v", version, tt.wantVersion)
			}
		})
	}
}

func TestSummarizePlan(t *testing.T) {
	tests := []struct {
		name        string
		plan        *PlanOutput
		wantCreate  int
		wantUpdate  int
		wantDelete  int
		wantReplace int
		wantTotal   int
	}{
		{
			name: "empty plan",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{},
			},
			wantCreate:  0,
			wantUpdate:  0,
			wantDelete:  0,
			wantReplace: 0,
			wantTotal:   0,
		},
		{
			name: "create only",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "test1", Change: Change{Actions: []string{"create"}}},
					{Address: "test2", Change: Change{Actions: []string{"create"}}},
				},
			},
			wantCreate:  2,
			wantUpdate:  0,
			wantDelete:  0,
			wantReplace: 0,
			wantTotal:   2,
		},
		{
			name: "update only",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "test1", Change: Change{Actions: []string{"update"}}},
				},
			},
			wantCreate:  0,
			wantUpdate:  1,
			wantDelete:  0,
			wantReplace: 0,
			wantTotal:   1,
		},
		{
			name: "delete only",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "test1", Change: Change{Actions: []string{"delete"}}},
					{Address: "test2", Change: Change{Actions: []string{"delete"}}},
					{Address: "test3", Change: Change{Actions: []string{"delete"}}},
				},
			},
			wantCreate:  0,
			wantUpdate:  0,
			wantDelete:  3,
			wantReplace: 0,
			wantTotal:   3,
		},
		{
			name: "replace (delete, create)",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "test1", Change: Change{Actions: []string{"delete", "create"}}},
				},
			},
			wantCreate:  0,
			wantUpdate:  0,
			wantDelete:  0,
			wantReplace: 1,
			wantTotal:   1,
		},
		{
			name: "replace (create, delete)",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "test1", Change: Change{Actions: []string{"create", "delete"}}},
				},
			},
			wantCreate:  0,
			wantUpdate:  0,
			wantDelete:  0,
			wantReplace: 1,
			wantTotal:   1,
		},
		{
			name: "mixed changes",
			plan: &PlanOutput{
				ResourceChanges: []ResourceChange{
					{Address: "create1", Change: Change{Actions: []string{"create"}}},
					{Address: "update1", Change: Change{Actions: []string{"update"}}},
					{Address: "delete1", Change: Change{Actions: []string{"delete"}}},
					{Address: "replace1", Change: Change{Actions: []string{"delete", "create"}}},
				},
			},
			wantCreate:  1,
			wantUpdate:  1,
			wantDelete:  1,
			wantReplace: 1,
			wantTotal:   4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := SummarizePlan(tt.plan)

			if summary.Create != tt.wantCreate {
				t.Errorf("SummarizePlan() Create = %v, want %v", summary.Create, tt.wantCreate)
			}
			if summary.Update != tt.wantUpdate {
				t.Errorf("SummarizePlan() Update = %v, want %v", summary.Update, tt.wantUpdate)
			}
			if summary.Delete != tt.wantDelete {
				t.Errorf("SummarizePlan() Delete = %v, want %v", summary.Delete, tt.wantDelete)
			}
			if summary.Replace != tt.wantReplace {
				t.Errorf("SummarizePlan() Replace = %v, want %v", summary.Replace, tt.wantReplace)
			}
			if summary.Total() != tt.wantTotal {
				t.Errorf("SummarizePlan() Total() = %v, want %v", summary.Total(), tt.wantTotal)
			}
		})
	}
}

func TestPlanSummary_Total(t *testing.T) {
	tests := []struct {
		name    string
		summary *PlanSummary
		want    int
	}{
		{
			name: "all zeros",
			summary: &PlanSummary{
				Create:  0,
				Update:  0,
				Delete:  0,
				Replace: 0,
			},
			want: 0,
		},
		{
			name: "all types",
			summary: &PlanSummary{
				Create:  1,
				Update:  2,
				Delete:  3,
				Replace: 4,
			},
			want: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.summary.Total(); got != tt.want {
				t.Errorf("PlanSummary.Total() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunner_ApplyWithRetry(t *testing.T) {
	tests := []struct {
		name       string
		failCount  int
		maxRetries int
		wantErr    bool
	}{
		{
			name:       "success on first try",
			failCount:  0,
			maxRetries: 3,
			wantErr:    false,
		},
		{
			name:       "success after retries",
			failCount:  2,
			maxRetries: 3,
			wantErr:    false,
		},
		{
			name:       "failure after max retries",
			failCount:  5,
			maxRetries: 3,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldExec := execCommandContext
			defer func() { execCommandContext = oldExec }()

			attemptCount := 0
			execCommandContext = func(ctx context.Context, command string, args ...string) *exec.Cmd {
				attemptCount++
				var exitErr error
				if attemptCount <= tt.failCount {
					exitErr = errors.New("exit status 1")
				}
				return mockExecCommand("apply output", exitErr)(ctx, command, args...)
			}

			logger := zaptest.NewLogger(t)
			runner := NewRunner(t.TempDir(), logger)

			err := runner.ApplyWithRetry(context.Background(), "/tmp/plan.out", tt.maxRetries, 10*time.Millisecond)
			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyWithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunner_ApplyWithRetry_ContextCancellation(t *testing.T) {
	oldExec := execCommandContext
	defer func() { execCommandContext = oldExec }()

	execCommandContext = mockExecCommand("", errors.New("exit status 1"))

	logger := zaptest.NewLogger(t)
	runner := NewRunner(t.TempDir(), logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := runner.ApplyWithRetry(ctx, "/tmp/plan.out", 3, 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ApplyWithRetry() error = %v, want context.Canceled", err)
	}
}

func TestRunner_command(t *testing.T) {
	logger := zaptest.NewLogger(t)
	dir := t.TempDir()
	runner := NewRunner(dir, logger)

	ctx := context.Background()
	cmd := runner.command(ctx, "plan", "-out=test")

	if cmd.Dir != dir {
		t.Errorf("command() Dir = %v, want /tmp/testdir", cmd.Dir)
	}

	if !strings.Contains(cmd.Path, "terraform") && cmd.Args[0] != os.Args[0] {
		t.Errorf("command() unexpected command path: %v", cmd.Path)
	}
}
