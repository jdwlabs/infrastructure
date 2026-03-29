package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"go.uber.org/zap"
)

// execCommandContext allows tests to mock command execution
var execCommandContext = exec.CommandContext

// LookPath checks if terraform is available in PATH
func LookPath() (string, error) {
	return exec.LookPath("terraform")
}

// Runner wraps terraform CLI operations
type Runner struct {
	dir    string
	logger *zap.Logger
	output io.Writer // writer for streaming command output (apply/destroy)
}

// NewRunner creates a new terraform runner for the given directory
func NewRunner(dir string, logger *zap.Logger) *Runner {
	return &Runner{dir: dir, logger: logger, output: os.Stdout}
}

// SetOutput sets the writer used for streaming terraform apply/destroy output.
// This allows callers to capture output in session logs.
func (r *Runner) SetOutput(w io.Writer) {
	r.output = w
}

// command builds an exec.Cmd rooted in the terraform directory
func (r *Runner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := execCommandContext(ctx, "terraform", args...)
	cmd.Dir = r.dir
	return cmd
}

// Init runs terraform init. Skips if .terraform/ already exists when skipIfExists is true.
func (r *Runner) Init(ctx context.Context) error {
	r.logger.Debug("running terraform init")
	cmd := r.command(ctx, "init")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("terraform init: %w\n%s", err, string(output))
	}
	return nil
}

// Fmt runs terraform fmt
func (r *Runner) Fmt(ctx context.Context) error {
	cmd := r.command(ctx, "fmt")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("terraform fmt: %w\n%s", err, string(output))
	}
	return nil
}

// Validate runs terraform validate
func (r *Runner) Validate(ctx context.Context) error {
	cmd := r.command(ctx, "validate")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("terraform validate: %w\n%s", err, string(output))
	}
	return nil
}

// Plan runs terraform plan and writes to the given plan file.
// Returns true if there are changes, false if infrastructure is up to date.
func (r *Runner) Plan(ctx context.Context, planFile string, extraArgs ...string) (bool, error) {
	args := []string{"plan", "-out=" + planFile, "-detailed-exitcode"}
	args = append(args, extraArgs...)

	cmd := r.command(ctx, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Exit code 2 means changes present
			if exitErr.ExitCode() == 2 {
				return true, nil
			}
		}
		return false, fmt.Errorf("terraform plan: %w\n%s", err, string(output))
	}
	// Exit code 0 means no changes
	return false, nil
}

// DestroyPlan runs terraform plan -destroy
func (r *Runner) DestroyPlan(ctx context.Context, planFile string, extraArgs ...string) error {
	args := []string{"plan", "-destroy", "-out=" + planFile}
	args = append(args, extraArgs...)

	cmd := r.command(ctx, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("terraform plan -destroy: %w\n%s", err, string(output))
	}
	return nil
}

// Apply runs terraform apply with the given plan file, streaming output to the configured writer.
func (r *Runner) Apply(ctx context.Context, planFile string, extraArgs ...string) error {
	args := []string{"apply"}
	args = append(args, extraArgs...)
	args = append(args, planFile)

	cmd := r.command(ctx, args...)
	cmd.Stdout = r.output
	cmd.Stderr = r.output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("terraform apply: %w", err)
	}
	return nil
}

// ApplyWithRetry runs terraform apply with retries and exponential backoff.
func (r *Runner) ApplyWithRetry(ctx context.Context, planFile string, maxRetries int, baseDelay time.Duration) error {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		r.logger.Info("terraform apply attempt", zap.Int("attempt", attempt), zap.Int("max", maxRetries))

		if err := r.Apply(ctx, planFile); err != nil {
			if attempt >= maxRetries {
				return fmt.Errorf("terraform apply failed after %d attempts: %w", maxRetries, err)
			}
			delay := baseDelay * time.Duration(attempt)
			r.logger.Warn("terraform apply failed, retrying",
				zap.Int("attempt", attempt),
				zap.Duration("delay", delay),
				zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("terraform apply: max retries exceeded")
}

// Destroy runs terraform destroy with auto-approve, streaming output to the configured writer.
func (r *Runner) Destroy(ctx context.Context, extraArgs ...string) error {
	args := []string{"destroy", "-auto-approve"}
	args = append(args, extraArgs...)

	cmd := r.command(ctx, args...)
	cmd.Stdout = r.output
	cmd.Stderr = r.output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("terraform destroy: %w", err)
	}
	return nil
}

// ShowJSON runs terraform show -json on a plan file and returns parsed output.
func (r *Runner) ShowJSON(ctx context.Context, planFile string) (*PlanOutput, error) {
	cmd := r.command(ctx, "show", "-json", planFile)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}

	var plan PlanOutput
	if err := json.Unmarshal(output, &plan); err != nil {
		return nil, fmt.Errorf("parse terraform plan JSON: %w", err)
	}
	return &plan, nil
}

// ShowStateJSON runs terraform show -json (no plan file) for current state.
func (r *Runner) ShowStateJSON(ctx context.Context) (*StateOutput, error) {
	cmd := r.command(ctx, "show", "-json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}

	var state StateOutput
	if err := json.Unmarshal(output, &state); err != nil {
		return nil, fmt.Errorf("parse terraform state JSON: %w", err)
	}
	return &state, nil
}

// StateList runs terraform state list and returns the resource addresses.
func (r *Runner) StateList(ctx context.Context) ([]string, error) {
	cmd := r.command(ctx, "state", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform state list: %w", err)
	}

	var resources []string
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		if s := string(bytes.TrimSpace(line)); s != "" {
			resources = append(resources, s)
		}
	}
	return resources, nil
}

// Version returns the terraform version string.
func (r *Runner) Version(ctx context.Context) (string, error) {
	cmd := r.command(ctx, "version", "-json")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("terraform version: %w", err)
	}

	var ver struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(output, &ver); err != nil {
		return "", fmt.Errorf("parse terraform version: %w", err)
	}
	return ver.Version, nil
}

// PlanOutput represents the JSON output of terraform show -json <planfile>
type PlanOutput struct {
	ResourceChanges []ResourceChange `json:"resource_changes"`
}

// ResourceChange represents a single resource change in a terraform plan
type ResourceChange struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Change  Change `json:"change"`
}

// Change represents the change details for a resource
type Change struct {
	Actions []string `json:"actions"`
}

// StateOutput represents terraform show -json output for current state
type StateOutput struct {
	Values *StateValues `json:"values"`
}

// StateValues contains the root module values
type StateValues struct {
	RootModule *RootModule `json:"root_module"`
}

// RootModule contains the resources in the root module
type RootModule struct {
	Resources []StateResource `json:"resources"`
}

// StateResource represents a resource in the terraform state
type StateResource struct {
	Address string                 `json:"address"`
	Type    string                 `json:"type"`
	Name    string                 `json:"name"`
	Values  map[string]interface{} `json:"values"`
}

// PlanSummary summarizes changes from a terraform plan
type PlanSummary struct {
	Create  int
	Update  int
	Delete  int
	Replace int
	Changes []ResourceChange
}

// SummarizePlan builds a summary from plan output.
// Terraform represents replacements as ["delete","create"] or ["create","delete"]
// action pairs on a single resource, not as a "replace" action string.
func SummarizePlan(plan *PlanOutput) *PlanSummary {
	s := &PlanSummary{Changes: plan.ResourceChanges}
	for _, rc := range plan.ResourceChanges {
		actions := rc.Change.Actions
		if len(actions) == 2 &&
			((actions[0] == "delete" && actions[1] == "create") ||
				(actions[0] == "create" && actions[1] == "delete")) {
			s.Replace++
			continue
		}
		for _, action := range actions {
			switch action {
			case "create":
				s.Create++
			case "update":
				s.Update++
			case "delete":
				s.Delete++
			}
		}
	}
	return s
}

// Total returns the total number of changes
func (s *PlanSummary) Total() int {
	return s.Create + s.Update + s.Delete + s.Replace
}
