package vllm

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleSpec() Spec {
	return Spec{
		Image: "docker.io/vllm/vllm-openai:v0.24.0@sha256:" + strings.Repeat("a", 64),
		Model: Model{
			Repo:     "QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ",
			Revision: strings.Repeat("b", 40),
		},
		ServedName: "qwen/qwen3-coder-30b-a3b",
		Port:       8000,
		Args:       []string{"--quantization=awq_marlin", "--max-model-len=32768"},
		HealthGate: Gate{Timeout: 10 * time.Minute},
	}
}

func TestExecArgsOrder(t *testing.T) {
	s := sampleSpec()
	want := []string{
		s.Model.Repo,
		"--revision", s.Model.Revision,
		"--served-model-name", s.ServedName,
		"--host", "0.0.0.0",
		"--port", "8000",
		"--quantization=awq_marlin",
		"--max-model-len=32768",
	}
	assert.Equal(t, want, ExecArgs(s))
}

func TestQuadletMatchesGolden(t *testing.T) {
	got := Quadlet(sampleSpec())
	golden, err := os.ReadFile("testdata/vllm-server.container.golden")
	require.NoError(t, err)
	assert.Equal(t, string(golden), got)
}

func TestArgsHashStableAndChangesWithAnyArg(t *testing.T) {
	base := sampleSpec()
	h1 := ArgsHash(base)
	h2 := ArgsHash(base)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64)

	mutations := map[string]func(*Spec){
		"model repo":  func(s *Spec) { s.Model.Repo = "Other/Model" },
		"revision":    func(s *Spec) { s.Model.Revision = strings.Repeat("c", 40) },
		"served name": func(s *Spec) { s.ServedName = "other/name" },
		"port":        func(s *Spec) { s.Port = 8001 },
		"args": func(s *Spec) {
			s.Args = append([]string{}, s.Args...)
			s.Args[0] = "--quantization=fp8"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := sampleSpec()
			mutate(&s)
			assert.NotEqual(t, h1, ArgsHash(s))
		})
	}
}

func TestNewAppliedJSONRoundTrips(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 30, 0, 123456000, time.UTC)
	applied := NewApplied(sampleSpec(), "deadbeefcafef00d", "jdwillmsen", at)

	data := applied.JSON()
	require.True(t, strings.HasSuffix(string(data), "\n"))

	var got Applied
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, applied.Commit, got.Commit)
	assert.Equal(t, applied.Image, got.Image)
	assert.Equal(t, applied.ImageDigest, got.ImageDigest)
	assert.Equal(t, applied.ModelRepo, got.ModelRepo)
	assert.Equal(t, applied.ModelRevision, got.ModelRevision)
	assert.Equal(t, applied.ServedName, got.ServedName)
	assert.Equal(t, applied.ArgsHash, got.ArgsHash)
	assert.Equal(t, applied.AppliedBy, got.AppliedBy)
	assert.True(t, applied.AppliedAt.Equal(got.AppliedAt))

	assert.NotEmpty(t, got.ImageDigest)
	assert.Equal(t, ArgsHash(sampleSpec()), got.ArgsHash)
}

func TestDriftFilesPathsAndModes(t *testing.T) {
	files := DriftFiles()
	require.Len(t, files, 3)

	byPath := map[string]int{}
	for i, f := range files {
		byPath[f.Path] = i
	}

	scriptIdx, ok := byPath["/usr/local/libexec/vllm-drift-check"]
	require.True(t, ok)
	assert.Equal(t, os.FileMode(0o755), files[scriptIdx].Mode)
	wantScript, err := os.ReadFile("assets/vllm-drift-check.sh")
	require.NoError(t, err)
	assert.Equal(t, wantScript, files[scriptIdx].Content)

	serviceIdx, ok := byPath["/etc/systemd/system/vllm-drift-check.service"]
	require.True(t, ok)
	wantService, err := os.ReadFile("assets/vllm-drift-check.service")
	require.NoError(t, err)
	assert.Equal(t, wantService, files[serviceIdx].Content)

	timerIdx, ok := byPath["/etc/systemd/system/vllm-drift-check.timer"]
	require.True(t, ok)
	wantTimer, err := os.ReadFile("assets/vllm-drift-check.timer")
	require.NoError(t, err)
	assert.Equal(t, wantTimer, files[timerIdx].Content)
}

// renderArgsInspectFormat executes ArgsInspectFormat with text/template —
// the same engine podman renders --format templates with — against the
// same shape podman's own inspect data has. A fixture (or assertion) built
// from this can't independently drift from whatever ArgsInspectFormat
// actually says, the way a hand-written stand-in could.
func renderArgsInspectFormat(t *testing.T, execArgs []string) []byte {
	t.Helper()
	tmpl := template.Must(template.New("args").Parse(ArgsInspectFormat))
	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, map[string]any{"Config": map[string]any{"Cmd": execArgs}}))
	return buf.Bytes()
}

// TestArgsInspectFormatOutputMatchesArgsHashInput pins the algebraic identity
// ArgsHash and the drift script both rely on, independent of any shell
// process: rendering ArgsInspectFormat, appending podman's own trailing
// "\n", and stripping the final byte (what `head -c -1` does) must equal
// strings.Join(ExecArgs(s), "\x00") exactly — no trailing NUL, no missing
// one.
func TestArgsInspectFormatOutputMatchesArgsHashInput(t *testing.T) {
	spec := sampleSpec()
	execArgs := ExecArgs(spec)

	rendered := renderArgsInspectFormat(t, execArgs)
	withPodmanNewline := append(rendered, '\n')
	stripped := withPodmanNewline[:len(withPodmanNewline)-1]

	assert.Equal(t, strings.Join(execArgs, "\x00"), string(stripped))
}

// headSupportsNegativeOffset reports whether `head -c -N` (GNU coreutils) is
// available: the drift script relies on it to strip podman's trailing "\n",
// and BSD head rejects a negative count outright.
func headSupportsNegativeOffset() bool {
	out, err := exec.Command("sh", "-c", "printf 'abc' | head -c -1").Output()
	return err == nil && string(out) == "ab"
}

// fakePodman is what the drift script's four podman invocations need
// answered: whether the container is running, its image ID, the RepoDigests
// of that image, and its argv.
type fakePodman struct {
	running     bool
	imageID     string
	repoDigests []string
	execArgs    []string
}

// writeFakePodman writes a stand-in `podman` binary to dir/podman. It
// matches each invocation on its exact argv — subcommand, --format string,
// and the "vllm"/image-id target — the same way real podman would be
// invoked, rather than on a loose substring of the format; anything it
// doesn't recognize fails loudly instead of silently returning nothing,
// since an empty answer to an unexpected call could pass as any reason.
func writeFakePodman(t *testing.T, dir string, f fakePodman) {
	t.Helper()

	runningStr := "false"
	if f.running {
		runningStr = "true"
	}

	var repoDigestLines strings.Builder
	for _, d := range f.repoDigests {
		repoDigestLines.WriteString("    printf '%s\\n' " + shellQuote(d) + "\n")
	}

	// Rendered by actually executing ArgsInspectFormat (see
	// renderArgsInspectFormat), not by hand-writing the NUL placement here —
	// so if ArgsInspectFormat (or the script's copy of it) is ever reverted
	// to the old, buggy shape, this fixture reverts with it and the "none"
	// case below stops matching ArgsHash, instead of a hand-written fake
	// silently going on producing the "right" bytes regardless.
	argsFixture := filepath.Join(dir, "args.out")
	argsOutput := renderArgsInspectFormat(t, f.execArgs)
	argsOutput = append(argsOutput, '\n') // podman's own trailing newline after the whole --format output
	require.NoError(t, os.WriteFile(argsFixture, argsOutput, 0o644))

	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = --format ] && [ \"$3\" = " + shellQuote("{{.State.Running}}") + " ] && [ \"$4\" = vllm ] && [ $# -eq 4 ]; then\n" +
		"    printf '%s\\n' " + shellQuote(runningStr) + "\n" +
		"    exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = --format ] && [ \"$3\" = " + shellQuote("{{.Image}}") + " ] && [ \"$4\" = vllm ] && [ $# -eq 4 ]; then\n" +
		"    printf '%s\\n' " + shellQuote(f.imageID) + "\n" +
		"    exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = image ] && [ \"$2\" = inspect ] && [ \"$3\" = --format ] && [ \"$4\" = " + shellQuote("{{range .RepoDigests}}{{println .}}{{end}}") + " ] && [ \"$5\" = " + shellQuote(f.imageID) + " ] && [ $# -eq 5 ]; then\n" +
		repoDigestLines.String() +
		"    exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = --format ] && [ \"$3\" = " + shellQuote(ArgsInspectFormat) + " ] && [ \"$4\" = vllm ] && [ $# -eq 4 ]; then\n" +
		"    cat " + shellQuote(argsFixture) + "\n" +
		"    exit 0\n" +
		"fi\n" +
		"echo \"fake podman: unexpected invocation: $*\" >&2\n" +
		"exit 1\n"

	require.NoError(t, os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755))
}

// writeFakePodmanNotFound writes a `podman` that always fails, standing in
// for a container that does not exist at all (as opposed to one that exists
// but is stopped).
func writeFakePodmanNotFound(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestDriftScriptContainsArgsInspectFormat(t *testing.T) {
	script, err := os.ReadFile("assets/vllm-drift-check.sh")
	require.NoError(t, err)
	assert.Contains(t, string(script), ArgsInspectFormat, "the script's podman --format string must be the exact same text as ArgsInspectFormat, or the two hashes are computed from different inputs")
}

func TestDriftCheckScriptReasons(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
	if !headSupportsNegativeOffset() {
		t.Skip("GNU coreutils `head -c -N` not available")
	}

	scriptPath := filepath.Join(t.TempDir(), "vllm-drift-check.sh")
	script, err := os.ReadFile("assets/vllm-drift-check.sh")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))

	spec := sampleSpec()
	execArgs := ExecArgs(spec)
	wantHash := ArgsHash(spec)
	at := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	record := NewApplied(spec, "deadbeefcafef00d", "jdwillmsen", at)
	require.Equal(t, wantHash, record.ArgsHash)

	const fakeImageID = "sha256-fake-image-id"
	matchingRepoDigest := "docker.io/vllm/vllm-openai@" + spec.ImageDigest()
	mismatchedRepoDigest := "docker.io/vllm/vllm-openai@sha256:" + strings.Repeat("f", 64)

	run := func(t *testing.T, binDir, appliedPath, textfileDir string) string {
		t.Helper()
		cmd := exec.Command("sh", scriptPath)
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"VLLM_APPLIED="+appliedPath,
			"VLLM_TEXTFILE_DIR="+textfileDir,
		)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "script output: %s", out)

		promPath := filepath.Join(textfileDir, "vllm_serving.prom")
		info, err := os.Stat(promPath)
		require.NoError(t, err)
		// node-exporter runs unprivileged and reads this file; mktemp's 0600
		// would make every metric in it silently absent.
		assert.Equalf(t, os.FileMode(0o644), info.Mode().Perm(), "mode of %s", promPath)

		data, err := os.ReadFile(promPath)
		require.NoError(t, err)
		return string(data)
	}

	writeRecord := func(t *testing.T, dir string) string {
		t.Helper()
		path := filepath.Join(dir, "applied.json")
		require.NoError(t, os.WriteFile(path, record.JSON(), 0o644))
		return path
	}

	assertReason := func(t *testing.T, prom, want string) {
		t.Helper()
		reasons := []string{"none", "image", "args", "not_running", "no_record"}
		for _, r := range reasons {
			wantVal := "0"
			if r == want {
				wantVal = "1"
			}
			assert.Containsf(t, prom, `vllm_serving_drift{reason="`+r+`"} `+wantVal, "reason=%s in:\n%s", r, prom)
		}
		assert.Contains(t, prom, "vllm_serving_info{")
		assert.Contains(t, prom, "vllm_serving_drift_check_timestamp_seconds ")
	}

	t.Run("no_record", func(t *testing.T) {
		binDir, textfileDir := t.TempDir(), t.TempDir()
		writeFakePodman(t, binDir, fakePodman{running: true, imageID: fakeImageID, repoDigests: []string{matchingRepoDigest}, execArgs: execArgs})
		appliedPath := filepath.Join(t.TempDir(), "missing.json")
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "no_record")
	})

	t.Run("not_running", func(t *testing.T) {
		binDir, textfileDir, recDir := t.TempDir(), t.TempDir(), t.TempDir()
		writeFakePodman(t, binDir, fakePodman{running: false, imageID: fakeImageID, repoDigests: []string{matchingRepoDigest}, execArgs: execArgs})
		appliedPath := writeRecord(t, recDir)
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "not_running")
	})

	t.Run("not_running (inspect fails outright)", func(t *testing.T) {
		binDir, textfileDir, recDir := t.TempDir(), t.TempDir(), t.TempDir()
		writeFakePodmanNotFound(t, binDir)
		appliedPath := writeRecord(t, recDir)
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "not_running")
	})

	t.Run("image", func(t *testing.T) {
		binDir, textfileDir, recDir := t.TempDir(), t.TempDir(), t.TempDir()
		writeFakePodman(t, binDir, fakePodman{running: true, imageID: fakeImageID, repoDigests: []string{mismatchedRepoDigest}, execArgs: execArgs})
		appliedPath := writeRecord(t, recDir)
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "image")
	})

	t.Run("args", func(t *testing.T) {
		binDir, textfileDir, recDir := t.TempDir(), t.TempDir(), t.TempDir()
		mutatedArgs := append([]string{}, execArgs...)
		mutatedArgs[len(mutatedArgs)-1] = "--max-model-len=1024"
		writeFakePodman(t, binDir, fakePodman{running: true, imageID: fakeImageID, repoDigests: []string{matchingRepoDigest}, execArgs: mutatedArgs})
		appliedPath := writeRecord(t, recDir)
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "args")
	})

	t.Run("none", func(t *testing.T) {
		binDir, textfileDir, recDir := t.TempDir(), t.TempDir(), t.TempDir()
		// A container's image typically carries several RepoDigests (one per
		// tag it was ever pulled/pushed under); the pinned digest only needs
		// to be among them, so a second, unrelated digest must not matter.
		writeFakePodman(t, binDir, fakePodman{running: true, imageID: fakeImageID, repoDigests: []string{mismatchedRepoDigest, matchingRepoDigest}, execArgs: execArgs})
		appliedPath := writeRecord(t, recDir)
		prom := run(t, binDir, appliedPath, textfileDir)
		assertReason(t, prom, "none")
		assert.Contains(t, prom, `served_name="`+spec.ServedName+`"`)
		assert.Contains(t, prom, `image_digest="`+spec.ImageDigest()+`"`)
	})
}
