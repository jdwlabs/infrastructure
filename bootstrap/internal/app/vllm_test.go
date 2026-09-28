package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/vllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// chdir switches the process working directory for the duration of the
// test; findRepoRoot and the git helpers both read os.Getwd()/run relative
// to the caller's directory.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// validSpecYAML is the minimal serving.yaml that passes vllm.Load's
// validation: a well-formed image reference, a 40-character revision, and a
// served name. Port and healthGate.timeout are left to their defaults.
func validSpecYAML() string {
	return "image: docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("a", 64) + "\n" +
		"model: {repo: Org/M, revision: " + strings.Repeat("b", 40) + "}\n" +
		"servedName: test-model\n"
}

func TestFindRepoRootWalksUpToTheDirectoryHoldingTerraform(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "terraform"), 0o755))
	deep := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	chdir(t, deep)

	got, err := findRepoRoot()
	require.NoError(t, err)

	wantAbs, _ := filepath.EvalSymlinks(root)
	gotAbs, _ := filepath.EvalSymlinks(got)
	assert.Equal(t, wantAbs, gotAbs)
}

func TestFindRepoRootFailsWhenNoTerraformDirectoryExists(t *testing.T) {
	chdir(t, t.TempDir())

	_, err := findRepoRoot()
	require.Error(t, err)
}

// vllmTestApp returns an App wired to a freshly written terraform.tfvars,
// so LoadTerraformExtras has something real to read.
func vllmTestApp(t *testing.T, tfvarsContent string) (*App, string) {
	t.Helper()
	tmpDir := t.TempDir()
	tfvarsPath := filepath.Join(tmpDir, "terraform.tfvars")
	require.NoError(t, os.WriteFile(tfvarsPath, []byte(tfvarsContent), 0o644))

	cfg := types.DefaultConfig()
	cfg.TerraformTFVars = tfvarsPath
	return &App{Cfg: cfg, Logger: zap.NewNop()}, tmpDir
}

func TestLoadVLLMContextPrefersOptsHostOverTFVars(t *testing.T) {
	a, tmpDir := vllmTestApp(t, `gpu_vm_ip = "10.0.0.9"`)
	specPath := filepath.Join(tmpDir, "serving.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(validSpecYAML()), 0o644))

	vctx, failure := a.loadVLLMContext(VLLMOptions{Host: "192.168.1.50", Spec: specPath})

	require.Nil(t, failure)
	assert.Equal(t, "192.168.1.50", vctx.host)
}

func TestLoadVLLMContextFallsBackToTFVarsGPUVMIPStrippingCIDR(t *testing.T) {
	a, tmpDir := vllmTestApp(t, `gpu_vm_ip = "10.0.0.9/24"`)
	specPath := filepath.Join(tmpDir, "serving.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(validSpecYAML()), 0o644))

	vctx, failure := a.loadVLLMContext(VLLMOptions{Spec: specPath})

	require.Nil(t, failure)
	assert.Equal(t, "10.0.0.9", vctx.host)
}

func TestLoadVLLMContextFailsWithVLLMHostUnset(t *testing.T) {
	a, _ := vllmTestApp(t, `cluster_name = "test"`)

	_, failure := a.loadVLLMContext(VLLMOptions{})

	require.NotNil(t, failure)
	assert.Equal(t, "vllm_host_unset", failure.Code)
	require.NotEmpty(t, helpForVLLMFailure(failure))
	assert.Contains(t, helpForVLLMFailure(failure)[0], "--host 192.168.1.50")
}

func TestLoadVLLMContextFailsWhenTFVarsMissing(t *testing.T) {
	a := &App{Cfg: types.DefaultConfig(), Logger: zap.NewNop()}
	a.Cfg.TerraformTFVars = filepath.Join(t.TempDir(), "does-not-exist.tfvars")

	_, failure := a.loadVLLMContext(VLLMOptions{Host: "192.168.1.50"})

	require.NotNil(t, failure)
	assert.Equal(t, "tfvars_not_found", failure.Code)
}

func TestLoadVLLMContextResolvesDefaultSpecPathUnderTheRepoRoot(t *testing.T) {
	a, _ := vllmTestApp(t, `gpu_vm_ip = "10.0.0.9"`)

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "terraform"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "inference", "vllm"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "inference", "vllm", "serving.yaml"), []byte(validSpecYAML()), 0o644))
	chdir(t, root)

	vctx, failure := a.loadVLLMContext(VLLMOptions{})

	require.Nil(t, failure)
	assert.Equal(t, filepath.Join(root, "inference", "vllm", "serving.yaml"), vctx.specPath)
	assert.Equal(t, "test-model", vctx.spec.ServedName)
}

func TestLoadVLLMContextFailsWithSpecUnreadableWhenServingYAMLIsMissing(t *testing.T) {
	a, _ := vllmTestApp(t, `gpu_vm_ip = "10.0.0.9"`)

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "terraform"), 0o755))
	chdir(t, root)

	_, failure := a.loadVLLMContext(VLLMOptions{})

	require.NotNil(t, failure)
	assert.Equal(t, "spec_unreadable", failure.Code)
	require.NotEmpty(t, helpForVLLMFailure(failure))
	assert.Contains(t, helpForVLLMFailure(failure)[0], "inference/vllm/serving.yaml")
}

func TestVLLMClientFallsBackToAgentAndFailsWithoutOne(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	a := &App{Logger: zap.NewNop()}
	cfg := types.DefaultConfig()
	cfg.ProxmoxSSHKeyPath = ""
	cfg.GPUVMSSHKeyPath = filepath.Join(t.TempDir(), "no-such-key")

	_, failure := a.vllmClient(cfg, "192.168.1.50")

	require.NotNil(t, failure)
	assert.Equal(t, "ssh_auth_unconfigured", failure.Code)
}

func TestOperatorIdentityIsUserAtHost(t *testing.T) {
	t.Setenv("USER", "jake")

	id := operatorIdentity()

	assert.True(t, strings.HasPrefix(id, "jake@"), "got %q", id)
}

// Status must fail the exit code on drift alone, with no Failure set —
// the report's drift field is the signal, and it must still print first.
func TestEmitVLLMStatusFailsTheExitCodeOnDriftAlone(t *testing.T) {
	a := &App{}
	buf := &bytes.Buffer{}

	err := a.emitVLLMStatus(VLLMOptions{Out: buf}, vllm.StatusResult{Host: "x", Drift: true})

	require.Error(t, err)
	assert.Contains(t, buf.String(), "drift: true")
}

func TestEmitVLLMStatusSucceedsWhenNotDrifted(t *testing.T) {
	a := &App{}
	buf := &bytes.Buffer{}

	err := a.emitVLLMStatus(VLLMOptions{Out: buf}, vllm.StatusResult{Host: "x", Drift: false})

	require.NoError(t, err)
}

// Unlike status, plan's drift ("changed") is the answer, not a failure.
func TestEmitVLLMPlanNeverFailsOnChangedAlone(t *testing.T) {
	a := &App{}
	buf := &bytes.Buffer{}

	err := a.emitVLLMPlan(VLLMOptions{Out: buf}, vllm.PlanResult{Host: "x", Changed: true})

	require.NoError(t, err)
}

func TestEmitVLLMApplyReturnsTheFailure(t *testing.T) {
	a := &App{}
	buf := &bytes.Buffer{}
	f := &vllm.Failure{Code: "gate_failed", Msg: "boom"}

	err := a.emitVLLMApply(VLLMOptions{Out: buf}, vllm.ApplyResult{Host: "x", Failure: f})

	require.Error(t, err)
	got, ok := err.(*vllm.Failure)
	require.True(t, ok)
	assert.Equal(t, "gate_failed", got.Code)
}

func TestHelpForVLLMFailureNamesTheFixingCommand(t *testing.T) {
	require.NotEmpty(t, helpForVLLMFailure(&vllm.Failure{Code: CodeConfirmRequired}))
	assert.Contains(t, helpForVLLMFailure(&vllm.Failure{Code: CodeConfirmRequired})[0], "talops vllm apply --confirm")

	require.NotEmpty(t, helpForVLLMFailure(&vllm.Failure{Code: "spec_unreadable"}))
	assert.Contains(t, helpForVLLMFailure(&vllm.Failure{Code: "spec_unreadable"})[0], "inference/vllm/serving.yaml")

	assert.Nil(t, helpForVLLMFailure(nil))
}

// --- apply's git checks, against a real temporary git repository ---
//
// These exercise the Critical fix directly: a relative --spec has to be
// checked against git as the exact file vllm.Load read, not as that
// relative string re-interpreted from repoRoot (which is what the process
// cwd was) — the two used to disagree whenever cwd != repoRoot, and an
// uncommitted spec could pass the dirty check silently.

// gitRepoFixture creates a real git repository with a terraform/ marker
// directory and a committed, tracked inference/vllm/serving.yaml — close
// enough to the real repo layout that findRepoRoot and the git checks run
// against real git plumbing rather than mocked command strings.
func gitRepoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	runGitFixture(t, root, "init", "-q")
	runGitFixture(t, root, "config", "user.email", "test@example.com")
	runGitFixture(t, root, "config", "user.name", "Test")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "terraform"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "inference", "vllm"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "inference", "vllm", "serving.yaml"), []byte(validSpecYAML()), 0o644))

	runGitFixture(t, root, "add", "-A")
	runGitFixture(t, root, "commit", "-q", "-m", "init")

	return root
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := runGit(dir, args...)
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// vllmApplyTestApp returns an App whose SSH client construction is
// guaranteed to fail (no key, no agent) so a test can read
// ssh_auth_unconfigured as proof every earlier check — including every git
// check — passed, without ever dialing a real host.
func vllmApplyTestApp(t *testing.T, root, gpuVMIP string) *App {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")

	tfvarsPath := filepath.Join(root, "terraform.tfvars")
	require.NoError(t, os.WriteFile(tfvarsPath, []byte(`gpu_vm_ip = "`+gpuVMIP+`"`), 0o644))

	cfg := types.DefaultConfig()
	cfg.TerraformTFVars = tfvarsPath
	cfg.ProxmoxSSHKeyPath = ""
	cfg.GPUVMSSHKeyPath = ""

	return &App{Cfg: cfg, Logger: zap.NewNop()}
}

func applyFailureCode(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	f, ok := err.(*vllm.Failure)
	require.True(t, ok, "expected a *vllm.Failure, got %T: %v", err, err)
	return f.Code
}

func TestVLLMApplyCleanSpecReachesTheHostStep(t *testing.T) {
	root := gitRepoFixture(t)
	chdir(t, root)
	a := vllmApplyTestApp(t, root, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true})

	assert.Equal(t, "ssh_auth_unconfigured", applyFailureCode(t, err),
		"a clean, tracked spec must pass every git check and reach host resolution")
}

func TestVLLMApplyRefusesADirtySpec(t *testing.T) {
	root := gitRepoFixture(t)
	specPath := filepath.Join(root, "inference", "vllm", "serving.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(validSpecYAML()+"# dirty\n"), 0o644))
	chdir(t, root)
	a := vllmApplyTestApp(t, root, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true})

	assert.Equal(t, "dirty_spec", applyFailureCode(t, err))
}

// The Critical regression case: --spec given as a relative path from a
// subdirectory used to be checked against git relative to repoRoot instead
// of relative to cwd, so an uncommitted file could read as clean.
func TestVLLMApplyCatchesADirtySpecGivenAsARelativePathFromASubdirectory(t *testing.T) {
	root := gitRepoFixture(t)
	specPath := filepath.Join(root, "inference", "vllm", "serving.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(validSpecYAML()+"# dirty\n"), 0o644))

	subdir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdir(t, subdir)

	relSpec, err := filepath.Rel(subdir, specPath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(relSpec, ".."),
		"the fixture must exercise a path that leaves the subdirectory, got %q", relSpec)

	a := vllmApplyTestApp(t, root, "10.0.0.9")

	runErr := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true, Spec: relSpec})

	assert.Equal(t, "dirty_spec", applyFailureCode(t, runErr),
		"a relative --spec from a subdirectory must resolve to the same tracked file vllm.Load read, and that file is dirty")
}

func TestVLLMApplyResolvesACleanRelativeSpecFromASubdirectory(t *testing.T) {
	root := gitRepoFixture(t)
	specPath := filepath.Join(root, "inference", "vllm", "serving.yaml")

	subdir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdir(t, subdir)

	relSpec, err := filepath.Rel(subdir, specPath)
	require.NoError(t, err)

	a := vllmApplyTestApp(t, root, "10.0.0.9")

	runErr := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true, Spec: relSpec})

	assert.Equal(t, "ssh_auth_unconfigured", applyFailureCode(t, runErr),
		"a clean, tracked spec resolved from a relative path must still pass every git check")
}

func TestVLLMApplyRefusesASpecOutsideTheRepo(t *testing.T) {
	root := gitRepoFixture(t)
	chdir(t, root)

	outside := t.TempDir()
	outsideSpec := filepath.Join(outside, "serving.yaml")
	require.NoError(t, os.WriteFile(outsideSpec, []byte(validSpecYAML()), 0o644))

	a := vllmApplyTestApp(t, root, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true, Spec: outsideSpec})

	assert.Equal(t, "spec_outside_repo", applyFailureCode(t, err))
}

func TestVLLMApplyRefusesAnUntrackedSpec(t *testing.T) {
	root := gitRepoFixture(t)
	chdir(t, root)

	untrackedSpec := filepath.Join(root, "inference", "vllm", "untracked.yaml")
	require.NoError(t, os.WriteFile(untrackedSpec, []byte(validSpecYAML()), 0o644))

	a := vllmApplyTestApp(t, root, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true, Spec: untrackedSpec})

	assert.Equal(t, "spec_untracked", applyFailureCode(t, err))
}

// The second Critical regression: git only ever tracks a symlink's target
// string, never its content, so a *tracked* spec that is itself a symlink
// to a file outside the repo would let an edit to that outside file take
// effect (Load follows the symlink) while git status on the symlink itself
// stays clean forever. The fix has to catch this by resolving to where the
// content actually lives, not by trusting a clean git status on the link.
func TestVLLMApplyRefusesASymlinkedSpecPointingOutsideTheRepo(t *testing.T) {
	root := gitRepoFixture(t)

	outside := t.TempDir()
	outsideSpec := filepath.Join(outside, "real-serving.yaml")
	require.NoError(t, os.WriteFile(outsideSpec, []byte(validSpecYAML()), 0o644))

	specPath := filepath.Join(root, "inference", "vllm", "serving.yaml")
	require.NoError(t, os.Remove(specPath))
	require.NoError(t, os.Symlink(outsideSpec, specPath))
	runGitFixture(t, root, "add", "-A")
	runGitFixture(t, root, "commit", "-q", "-m", "spec becomes a symlink to outside content")

	chdir(t, root)
	a := vllmApplyTestApp(t, root, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true})
	assert.Equal(t, "spec_outside_repo", applyFailureCode(t, err),
		"a tracked symlink whose target lies outside the repo must still be refused")

	// git status on the symlink itself never changes here — only the
	// outside file's content does — which is exactly what made this
	// dangerous: a git-invisible edit that Load would still pick up.
	require.NoError(t, os.WriteFile(outsideSpec, []byte(validSpecYAML()+"# edited outside the repo, invisible to git\n"), 0o644))

	err = a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true})
	assert.Equal(t, "spec_outside_repo", applyFailureCode(t, err),
		"an edit to the symlink's outside target must not be applied just because the symlink itself is clean")
}

// A repo reached through a symlinked directory (e.g. a worktree symlinked
// from elsewhere) must not be treated as if the spec were outside it —
// resolving repoRoot has to keep pace with resolving specPath.
func TestVLLMApplyThroughASymlinkedRepoRootStillPassesGitChecks(t *testing.T) {
	realRoot := gitRepoFixture(t)

	linked := filepath.Join(filepath.Dir(realRoot), "linked-"+filepath.Base(realRoot))
	require.NoError(t, os.Symlink(realRoot, linked))
	t.Cleanup(func() { _ = os.Remove(linked) })

	chdir(t, linked)
	a := vllmApplyTestApp(t, realRoot, "10.0.0.9")

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true})

	assert.Equal(t, "ssh_auth_unconfigured", applyFailureCode(t, err),
		"a clean spec reached through a symlinked repo root must still pass every git check")
}

// applyNotes runs apply against the fixture as far as the host step and
// returns the notes its report carried.
func applyNotes(t *testing.T, root string) []string {
	t.Helper()
	chdir(t, root)
	a := vllmApplyTestApp(t, root, "10.0.0.9")
	buf := &bytes.Buffer{}

	err := a.RunVLLMApply(context.Background(), VLLMOptions{Confirm: true, JSON: true, Out: buf})
	require.Equal(t, "ssh_auth_unconfigured", applyFailureCode(t, err))

	var got struct {
		Notes []string `json:"notes"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got), buf.String())
	return got.Notes
}

func TestVLLMApplyNotesACommitNotOnOriginMain(t *testing.T) {
	t.Run("HEAD is on origin/main", func(t *testing.T) {
		root := gitRepoFixture(t)
		runGitFixture(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
		assert.Empty(t, applyNotes(t, root))
	})

	t.Run("HEAD is ahead of origin/main", func(t *testing.T) {
		root := gitRepoFixture(t)
		runGitFixture(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
		runGitFixture(t, root, "commit", "-q", "--allow-empty", "-m", "unmerged")
		notes := applyNotes(t, root)
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "not on origin/main")
	})

	t.Run("no origin/main to compare against", func(t *testing.T) {
		root := gitRepoFixture(t)
		notes := applyNotes(t, root)
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "origin/main")
	})
}
