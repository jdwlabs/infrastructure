package vllm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner is the same shape hostconverge's tests use: it records every
// command, fails any whose text contains a key of fail, and returns the
// scripted output for the first key of out it contains. Every test in this
// file picks fail/out keys that are substrings of exactly one command it
// issues, so which key wins isn't sensitive to Go's randomized map order.
type fakeRunner struct {
	cmds []string
	fail map[string]error
	out  map[string]string
}

func (f *fakeRunner) Run(ctx context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	for k, err := range f.fail {
		if strings.Contains(cmd, k) {
			return "", err
		}
	}
	for k, o := range f.out {
		if strings.Contains(cmd, k) {
			return o, nil
		}
	}
	return "", nil
}

// scriptedOutErrRunner answers every command with a fixed output AND a
// fixed error, which fakeRunner's fail map can't do (a matched fail key
// always answers with ""): LegacyActive needs this to prove a recognised
// status string wins even when the runner also reports the command failed,
// the way systemctl is-active does for every state but "active".
type scriptedOutErrRunner struct {
	out string
	err error
}

func (s scriptedOutErrRunner) Run(ctx context.Context, cmd string) (string, error) {
	return s.out, s.err
}

var errFail = errFailT{}

type errFailT struct{}

func (errFailT) Error() string { return "fake failure" }

func indexOf(cmds []string, want string) int {
	for i, c := range cmds {
		if c == want {
			return i
		}
	}
	return -1
}

// ---- exact command text, mirrored from remote.go so tests fail loudly if
// the two ever drift instead of silently matching a looser substring ----

const (
	wantDpkgQueryCmd         = "dpkg-query -W -f='${Package} ${db:Status-Status}\\n' podman nvidia-container-toolkit prometheus-node-exporter 2>/dev/null"
	wantAptGetInstallCmd     = "sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y podman nvidia-container-toolkit prometheus-node-exporter"
	wantAptCachePolicyCmd    = "apt-cache policy nvidia-container-toolkit 2>/dev/null"
	wantMkdirCmd             = "sudo mkdir -p /usr/local/libexec /etc/containers/systemd /var/lib/vllm/hf /etc/vllm /var/lib/prometheus/node-exporter"
	wantNvidiaSmiCmd         = "nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null"
	wantCDIRecordReadCmd     = "sudo cat /etc/vllm/cdi-driver-version 2>/dev/null"
	wantNvidiaCtkGenerateCmd = "sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
	wantNodeExporterGrepCmd  = "grep '^ARGS=' /etc/default/prometheus-node-exporter 2>/dev/null"

	wantDfVllmCmd       = "df --output=avail -B1 /var/lib/vllm 2>/dev/null | tail -1"
	wantDfContainersCmd = "df --output=avail -B1 /var/lib/containers 2>/dev/null | tail -1"

	wantRunningCmd = "sudo podman inspect --format '{{.State.Running}}' vllm 2>/dev/null"
	wantImageIDCmd = "sudo podman inspect --format '{{.Image}}' vllm 2>/dev/null"

	wantProbeAppliedCmd = "sudo sh -c 'test -e /etc/vllm/applied.json || echo absent'"
	wantCatAppliedCmd   = "sudo cat /etc/vllm/applied.json 2>/dev/null"

	wantIsActiveCmd = "systemctl is-active vllm.service 2>/dev/null"
)

func wantCDIRecordWriteCmd(version string) string {
	return "printf '%s' '" + version + "' | sudo tee /etc/vllm/cdi-driver-version >/dev/null"
}

func wantMarkerProbeCmd(rev string) string { return "sudo test -e /var/lib/vllm/hf/.staged-" + rev }
func wantTouchMarkerCmd(rev string) string { return "sudo touch /var/lib/vllm/hf/.staged-" + rev }
func wantPullCmd(image string) string      { return "sudo podman pull '" + image + "'" }
func wantDownloadCmd(image, repo, rev string) string {
	return fmt.Sprintf(
		"sudo podman run --rm --entrypoint python3 -v /var/lib/vllm/hf:/root/.cache/huggingface '%s' -c \"from huggingface_hub import snapshot_download; snapshot_download('%s', revision='%s')\"",
		image, repo, rev,
	)
}
func wantRepoDigestsCmd(imageID string) string {
	return "sudo podman image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' " + imageID + " 2>/dev/null"
}
func wantArgsCmd() string {
	return "sudo podman inspect --format '" + ArgsInspectFormat + "' vllm 2>/dev/null"
}

// ---- EnsureHost ----

// allInstalledOK scripts a fakeRunner where every package is installed, the
// CDI record matches nvidia-smi's driver version, and node-exporter's
// config already matches — the baseline every EnsureHost test starts from
// and overrides pieces of.
func allInstalledOK() *fakeRunner {
	return &fakeRunner{
		out: map[string]string{
			"dpkg-query":                            "podman installed\nnvidia-container-toolkit installed\nprometheus-node-exporter installed\n",
			"nvidia-smi":                            "550.90.07\n",
			"sudo cat /etc/vllm/cdi-driver-version": "550.90.07",
			"/etc/default/prometheus-node-exporter": "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter\"\n",
		},
	}
}

func TestEnsureHostAllPresentAndMatchingIsANoOp(t *testing.T) {
	r := allInstalledOK()
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNodeExporterGrepCmd,
	}, r.cmds)
}

func TestEnsureHostInstallsMissingPackages(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = "podman installed\nprometheus-node-exporter installed\n" // nvidia-container-toolkit missing
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantAptGetInstallCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{"installed podman, nvidia-container-toolkit, prometheus-node-exporter"}, changed)
}

// TestEnsureHostDuplicateInstalledLineDoesNotMaskAMissingPackage kills the
// "dpkg line-count" mutant: three "installed" lines is coincidentally the
// right count, but two of them are the same package repeated, so
// prometheus-node-exporter was never actually reported.
func TestEnsureHostDuplicateInstalledLineDoesNotMaskAMissingPackage(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = "podman installed\npodman installed\nnvidia-container-toolkit installed\n"
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Contains(t, r.cmds, wantAptGetInstallCmd)
	assert.Equal(t, []string{"installed podman, nvidia-container-toolkit, prometheus-node-exporter"}, changed)
}

func TestEnsureHostNoAptCandidateReturnsDocumentedErrorAndSkipsCDIAndMkdir(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = "nvidia-container-toolkit:\n  Installed: (none)\n  Candidate: (none)\n  Version table:\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NVIDIA Container Toolkit apt repository")
	assert.Contains(t, err.Error(), "docs/vllm-serving.md#host-prerequisites")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantAptGetInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

func TestEnsureHostEmptyPolicyOutputReturnsDocumentedError(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = ""

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no apt installation candidate")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantAptGetInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

func TestEnsureHostInstallFailsWithCandidatePresentReturnsGenericError(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = "nvidia-container-toolkit:\n  Installed: 1.13.5-1\n  Candidate: 1.13.5-1\n  Version table:\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "docs/vllm-serving.md#host-prerequisites")
	assert.Contains(t, err.Error(), "install host packages")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantAptGetInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

func TestEnsureHostAptCachePolicyErrorWrapsBothErrors(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{
		"apt-get install":  errFail,
		"apt-cache policy": errFail,
	}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install host packages")
	assert.NotContains(t, err.Error(), "docs/vllm-serving.md#host-prerequisites")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantAptGetInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

// TestEnsureHostMkdirFailurePropagates kills the "mkdir failure" mutant and
// also proves N1's ordering: mkdir runs right after the install check, so
// its failure stops everything before nvidia-smi or the CDI block ever run.
func TestEnsureHostMkdirFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.fail = map[string]error{"mkdir -p": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create host directories")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantMkdirCmd}, r.cmds)
}

// TestEnsureHostCreatesDirectoriesBeforeWritingCDIRecord pins N1: mkdir
// must run before the CDI record write, or a fresh host (no /etc/vllm yet)
// fails permanently the first time the CDI record needs writing.
func TestEnsureHostCreatesDirectoriesBeforeWritingCDIRecord(t *testing.T) {
	r := allInstalledOK()
	r.out["sudo cat /etc/vllm/cdi-driver-version"] = "550.54.15" // forces regenerate + write

	_, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)

	mkdirIdx := indexOf(r.cmds, wantMkdirCmd)
	writeIdx := indexOf(r.cmds, wantCDIRecordWriteCmd("550.90.07"))
	require.GreaterOrEqual(t, mkdirIdx, 0, "mkdir never ran: %v", r.cmds)
	require.GreaterOrEqual(t, writeIdx, 0, "CDI record was never written: %v", r.cmds)
	assert.Less(t, mkdirIdx, writeIdx, "mkdir must run before the CDI record write")
}

func TestEnsureHostNvidiaSmiFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.fail = map[string]error{"nvidia-smi": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read GPU driver version")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantMkdirCmd, wantNvidiaSmiCmd}, r.cmds)
}

func TestEnsureHostNvidiaSmiEmptyOutputIsAnError(t *testing.T) {
	r := allInstalledOK()
	r.out["nvidia-smi"] = "\n\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no driver version")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantMkdirCmd, wantNvidiaSmiCmd}, r.cmds)
}

// TestEnsureHostMalformedDriverVersionIsRejected covers N3: a driver
// version that doesn't look like dotted digits is refused before it's
// interpolated into the CDI record write command.
func TestEnsureHostMalformedDriverVersionIsRejected(t *testing.T) {
	r := allInstalledOK()
	r.out["nvidia-smi"] = "not-a-version; rm -rf /\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed driver version")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantMkdirCmd, wantNvidiaSmiCmd}, r.cmds)
}

// TestEnsureHostUsesFirstNonEmptyDriverVersionLine kills the "multi-GPU
// uses the wrong line" mutant: a two-GPU host reports one line per card,
// and only the first is used.
func TestEnsureHostUsesFirstNonEmptyDriverVersionLine(t *testing.T) {
	r := allInstalledOK()
	r.out["nvidia-smi"] = "550.90.07\n999.99.99\n"
	r.out["sudo cat /etc/vllm/cdi-driver-version"] = "999.99.99" // mismatches the SECOND line, matches neither if the first is used correctly... see below

	_, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Contains(t, r.cmds, wantCDIRecordWriteCmd("550.90.07"), "must record the FIRST GPU's driver version, not the second")
}

func TestEnsureHostRegeneratesCDIWhenDriverVersionDiffers(t *testing.T) {
	r := allInstalledOK()
	r.out["sudo cat /etc/vllm/cdi-driver-version"] = "550.54.15"

	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRecordWriteCmd("550.90.07"),
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{"regenerated /etc/cdi/nvidia.yaml for driver 550.90.07"}, changed)
}

func TestEnsureHostRegeneratesCDIWhenRecordMissing(t *testing.T) {
	r := allInstalledOK()
	// Matches only the read ("sudo cat ...cdi-driver-version"), not the
	// write ("... | sudo tee ...cdi-driver-version"): failing both would
	// turn this into an unrelated "record CDI driver version" error.
	r.fail = map[string]error{"sudo cat /etc/vllm/cdi-driver-version": errFail}

	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRecordWriteCmd("550.90.07"),
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{"regenerated /etc/cdi/nvidia.yaml for driver 550.90.07"}, changed)
}

// TestEnsureHostNvidiaCtkGenerateFailurePropagates kills the "nvidia-ctk
// failure ignored" mutant: a failed generate must never be followed by a
// write of the driver-version record it didn't actually produce.
func TestEnsureHostNvidiaCtkGenerateFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.out["sudo cat /etc/vllm/cdi-driver-version"] = "550.54.15" // forces a regenerate attempt
	r.fail = map[string]error{"nvidia-ctk cdi generate": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generate CDI spec")
	assert.NotContains(t, r.cmds, wantCDIRecordWriteCmd("550.90.07"))
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
	}, r.cmds)
}

func TestEnsureHostReportsNodeExporterTextfileMismatch(t *testing.T) {
	r := allInstalledOK()
	r.out["/etc/default/prometheus-node-exporter"] = "ARGS=\"--collector.textfile.directory=/some/other/dir\"\n"

	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNodeExporterGrepCmd,
	}, r.cmds)
	require.Len(t, changed, 1)
	assert.Contains(t, changed[0], "warning: ")
	assert.Contains(t, changed[0], "/etc/default/prometheus-node-exporter")
}

// ---- Stage ----

func stageSpec() Spec {
	return sampleSpec()
}

func TestStageUnstagedChecksBothPathsAndDownloads(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		fail: map[string]error{"test -e /var/lib/vllm/hf/.staged-": errFail}, // marker absent
		out: map[string]string{
			"df --output=avail -B1 /var/lib/vllm":       "32212254720", // exactly 30 GiB
			"df --output=avail -B1 /var/lib/containers": "12884901888", // exactly 12 GiB
		},
	}
	err := Stage(context.Background(), r, spec)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantMarkerProbeCmd(spec.Model.Revision),
		wantDfVllmCmd,
		wantDfContainersCmd,
		wantPullCmd(spec.Image),
		wantDownloadCmd(spec.Image, spec.Model.Repo, spec.Model.Revision),
		wantTouchMarkerCmd(spec.Model.Revision),
	}, r.cmds)
}

func TestStageStagedSkipsModelCheckAndDownload(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		// marker probe defaults to success (staged)
		out: map[string]string{"df --output=avail -B1 /var/lib/containers": "12884901888"},
	}
	err := Stage(context.Background(), r, spec)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantMarkerProbeCmd(spec.Model.Revision),
		wantDfContainersCmd,
		wantPullCmd(spec.Image),
	}, r.cmds)
}

func TestStageRefusesWhenModelDiskLowNamesShortfall(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		fail: map[string]error{"test -e /var/lib/vllm/hf/.staged-": errFail}, // marker absent -> 30 GiB required
		out:  map[string]string{"df --output=avail -B1 /var/lib/vllm": "10737418240"},
	}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/var/lib/vllm")
	assert.Contains(t, err.Error(), "10 GiB free")
	assert.Contains(t, err.Error(), "20 GiB short")
	assert.Contains(t, err.Error(), "30 GiB required")
	assert.Equal(t, []string{wantMarkerProbeCmd(spec.Model.Revision), wantDfVllmCmd}, r.cmds)
}

func TestStageRefusesWhenImageDiskLowNamesShortfall(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		// marker probe defaults to success (staged), so only the image
		// check on /var/lib/containers runs.
		out: map[string]string{"df --output=avail -B1 /var/lib/containers": "1073741824"}, // 1 GiB
	}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/var/lib/containers")
	assert.Contains(t, err.Error(), "1 GiB free")
	assert.Contains(t, err.Error(), "11 GiB short")
	assert.Contains(t, err.Error(), "12 GiB required")
	assert.Equal(t, []string{wantMarkerProbeCmd(spec.Model.Revision), wantDfContainersCmd}, r.cmds)
}

func TestStageDfParseFailureReturnsError(t *testing.T) {
	spec := stageSpec()
	// marker probe defaults to success (staged), so only /var/lib/containers is checked.
	r := &fakeRunner{out: map[string]string{"df --output=avail -B1 /var/lib/containers": "not-a-number"}}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse free space on /var/lib/containers")
	assert.Equal(t, []string{wantMarkerProbeCmd(spec.Model.Revision), wantDfContainersCmd}, r.cmds)
}

func TestStagePullFailurePropagates(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		fail: map[string]error{"podman pull": errFail},
		out:  map[string]string{"df --output=avail -B1 /var/lib/containers": "12884901888"},
	}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull image")
	assert.Equal(t, []string{
		wantMarkerProbeCmd(spec.Model.Revision),
		wantDfContainersCmd,
		wantPullCmd(spec.Image),
	}, r.cmds)
}

func TestStageDownloadFailurePropagatesAndSkipsMarker(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		fail: map[string]error{
			"test -e /var/lib/vllm/hf/.staged-": errFail,
			"snapshot_download":                 errFail,
		},
		out: map[string]string{
			"df --output=avail -B1 /var/lib/vllm":       "32212254720",
			"df --output=avail -B1 /var/lib/containers": "12884901888",
		},
	}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "download model")
	assert.NotContains(t, r.cmds, wantTouchMarkerCmd(spec.Model.Revision))
	assert.Equal(t, []string{
		wantMarkerProbeCmd(spec.Model.Revision),
		wantDfVllmCmd,
		wantDfContainersCmd,
		wantPullCmd(spec.Image),
		wantDownloadCmd(spec.Image, spec.Model.Repo, spec.Model.Revision),
	}, r.cmds)
}

// TestStageMarkerTouchFailurePropagates kills the "marker-touch failure
// ignored" mutant: if the marker can't actually be written, Stage must
// fail rather than report success (which would read as "staged" forever
// even though the marker never landed).
func TestStageMarkerTouchFailurePropagates(t *testing.T) {
	spec := stageSpec()
	r := &fakeRunner{
		fail: map[string]error{
			"test -e /var/lib/vllm/hf/.staged-":    errFail,
			"sudo touch /var/lib/vllm/hf/.staged-": errFail,
		},
		out: map[string]string{
			"df --output=avail -B1 /var/lib/vllm":       "32212254720",
			"df --output=avail -B1 /var/lib/containers": "12884901888",
		},
	}
	err := Stage(context.Background(), r, spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "record staged marker")
	assert.Equal(t, []string{
		wantMarkerProbeCmd(spec.Model.Revision),
		wantDfVllmCmd,
		wantDfContainersCmd,
		wantPullCmd(spec.Image),
		wantDownloadCmd(spec.Image, spec.Model.Repo, spec.Model.Revision),
		wantTouchMarkerCmd(spec.Model.Revision),
	}, r.cmds)
}

// ---- ReadLive ----

func TestReadLiveNotRunningOnInspectFailure(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"{{.State.Running}}": errFail}}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd}, r.cmds)
}

func TestReadLiveNotRunningWhenStateFalse(t *testing.T) {
	r := &fakeRunner{out: map[string]string{"{{.State.Running}}": "false\n"}}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd}, r.cmds)
}

func TestReadLiveNotRunningWhenImageInspectFails(t *testing.T) {
	r := &fakeRunner{
		out:  map[string]string{"{{.State.Running}}": "true\n"},
		fail: map[string]error{"{{.Image}}": errFail},
	}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd, wantImageIDCmd}, r.cmds)
}

func TestReadLiveNotRunningWhenImageIDIsMalformed(t *testing.T) {
	r := &fakeRunner{
		out: map[string]string{
			"{{.State.Running}}": "true\n",
			"{{.Image}}":         "not-a-real-image-id\n",
		},
	}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd, wantImageIDCmd}, r.cmds)
}

func TestReadLiveNotRunningWhenRepoDigestsInspectFails(t *testing.T) {
	imageID := strings.Repeat("c", 64)
	r := &fakeRunner{
		out: map[string]string{
			"{{.State.Running}}": "true\n",
			"{{.Image}}":         imageID + "\n",
		},
		fail: map[string]error{"{{range .RepoDigests}}": errFail},
	}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd, wantImageIDCmd, wantRepoDigestsCmd(imageID)}, r.cmds)
}

func TestReadLiveNotRunningWhenArgsInspectFails(t *testing.T) {
	imageID := strings.Repeat("c", 64)
	r := &fakeRunner{
		out: map[string]string{
			"{{.State.Running}}": "true\n",
			"{{.Image}}":         imageID + "\n",
			"{{range .RepoDigests}}{{println .}}{{end}}": "docker.io/vllm/vllm-openai@sha256:" + strings.Repeat("d", 64) + "\n",
		},
		fail: map[string]error{ArgsInspectFormat: errFail},
	}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, Live{}, live)
	assert.Equal(t, []string{wantRunningCmd, wantImageIDCmd, wantRepoDigestsCmd(imageID), wantArgsCmd()}, r.cmds)
}

func TestReadLiveComputesRepoDigestsAndArgsHash(t *testing.T) {
	spec := sampleSpec()
	execArgs := ExecArgs(spec)
	argsOut := renderArgsInspectFormat(t, execArgs)
	argsOut = append(argsOut, '\n')
	imageID := strings.Repeat("c", 64)

	r := &fakeRunner{
		out: map[string]string{
			"{{.State.Running}}": "true\n",
			"{{.Image}}":         imageID + "\n",
			"{{range .RepoDigests}}{{println .}}{{end}}": "docker.io/vllm/vllm-openai@" + spec.ImageDigest() + "\n",
			ArgsInspectFormat: string(argsOut),
		},
	}
	live, err := ReadLive(context.Background(), r)
	require.NoError(t, err)
	assert.True(t, live.Running)
	assert.True(t, live.HasDigest(spec.ImageDigest()))
	assert.False(t, live.HasDigest("sha256:"+strings.Repeat("f", 64)))

	wantHashInput := strings.TrimSuffix(string(argsOut), "\n")
	wantSum := sha256.Sum256([]byte(wantHashInput))
	assert.Equal(t, hex.EncodeToString(wantSum[:]), live.ArgsHash)
	assert.Equal(t, ArgsHash(spec), live.ArgsHash)

	assert.Equal(t, []string{wantRunningCmd, wantImageIDCmd, wantRepoDigestsCmd(imageID), wantArgsCmd()}, r.cmds)
}

func TestHasDigestMatchesBySuffix(t *testing.T) {
	l := Live{RepoDigests: []string{"docker.io/x@sha256:" + strings.Repeat("a", 64), "docker.io/y@sha256:" + strings.Repeat("b", 64)}}
	assert.True(t, l.HasDigest("sha256:"+strings.Repeat("b", 64)))
	assert.False(t, l.HasDigest("sha256:"+strings.Repeat("c", 64)))
}

// ---- ReadApplied ----

func TestReadAppliedReturnsNilNilWhenAbsent(t *testing.T) {
	r := &fakeRunner{out: map[string]string{"test -e /etc/vllm/applied.json": "absent\n"}}
	applied, err := ReadApplied(context.Background(), r)
	require.NoError(t, err)
	assert.Nil(t, applied)
	assert.Equal(t, []string{wantProbeAppliedCmd}, r.cmds)
}

func TestReadAppliedProbeErrorPropagates(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"test -e /etc/vllm/applied.json": errFail}}
	applied, err := ReadApplied(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check applied record")
	assert.Nil(t, applied)
	assert.Equal(t, []string{wantProbeAppliedCmd}, r.cmds)
}

func TestReadAppliedParsesRecord(t *testing.T) {
	spec := sampleSpec()
	record := NewApplied(spec, "deadbeef", "jdwillmsen", time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC))
	r := &fakeRunner{out: map[string]string{"cat /etc/vllm/applied.json": string(record.JSON())}}
	applied, err := ReadApplied(context.Background(), r)
	require.NoError(t, err)
	require.NotNil(t, applied)
	assert.Equal(t, "deadbeef", applied.Commit)
	assert.Equal(t, spec.ImageDigest(), applied.ImageDigest)
	assert.Equal(t, []string{wantProbeAppliedCmd, wantCatAppliedCmd}, r.cmds)
}

func TestReadAppliedCatFailureReturnsError(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"cat /etc/vllm/applied.json": errFail}}
	applied, err := ReadApplied(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read applied record")
	assert.Nil(t, applied)
	assert.Equal(t, []string{wantProbeAppliedCmd, wantCatAppliedCmd}, r.cmds)
}

func TestReadAppliedMalformedJSONReturnsError(t *testing.T) {
	r := &fakeRunner{out: map[string]string{"cat /etc/vllm/applied.json": "{not valid json"}}
	applied, err := ReadApplied(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse applied record")
	assert.Nil(t, applied)
	assert.Equal(t, []string{wantProbeAppliedCmd, wantCatAppliedCmd}, r.cmds)
}

// ---- LegacyActive ----

func TestLegacyActiveRecognizedStates(t *testing.T) {
	cases := map[string]bool{
		"active":       true,
		"activating":   true,
		"reloading":    true,
		"deactivating": true,
		"inactive":     false,
		"failed":       false,
		"unknown":      false,
		"maintenance":  false,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			r := &fakeRunner{out: map[string]string{"is-active vllm.service": status + "\n"}}
			active, err := LegacyActive(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, want, active)
			assert.Equal(t, []string{wantIsActiveCmd}, r.cmds)
		})
	}
}

func TestLegacyActiveRecognizedOutputWinsOverRunnerError(t *testing.T) {
	r := scriptedOutErrRunner{out: "deactivating\n", err: errFail}
	active, err := LegacyActive(context.Background(), r)
	require.NoError(t, err)
	assert.True(t, active)
}

func TestLegacyActiveUnrecognizedOutputWithRunnerErrorIsAnError(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"is-active vllm.service": errFail}}
	active, err := LegacyActive(context.Background(), r)
	require.Error(t, err)
	assert.False(t, active)
	assert.Contains(t, err.Error(), "check legacy vllm.service")
}

// TestLegacyActiveUnrecognizedOutputWithNilErrorIsAnError covers N2: an
// output that's neither a known-active nor a known-inactive status must
// never be silently read as "not active", even when the runner itself
// reported no error.
func TestLegacyActiveUnrecognizedOutputWithNilErrorIsAnError(t *testing.T) {
	r := scriptedOutErrRunner{out: "some-future-systemd-state\n", err: nil}
	active, err := LegacyActive(context.Background(), r)
	require.Error(t, err)
	assert.False(t, active)
	assert.Contains(t, err.Error(), "check legacy vllm.service")
	assert.Contains(t, err.Error(), "some-future-systemd-state")
}
