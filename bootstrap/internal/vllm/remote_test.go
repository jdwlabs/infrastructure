package vllm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// A successful `sudo rm -f <path>` forgets every scripted output keyed on
// that path, so a later read of the removed file comes back empty.
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
	if path, ok := strings.CutPrefix(cmd, "sudo rm -f "); ok {
		for k := range f.out {
			if strings.Contains(k, path) {
				delete(f.out, k)
			}
		}
		return "", nil
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
// always answers with ""): LegacyPresent needs this to prove a recognised
// state wins even when the runner reports no error alongside it.
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
	wantDpkgQueryCmd         = "dpkg-query -W -f='${Package} ${Version} ${db:Status-Want} ${db:Status-Status}\\n' podman prometheus-node-exporter nvidia-container-toolkit nvidia-container-toolkit-base libnvidia-container-tools libnvidia-container1 2>/dev/null || test $? -eq 1"
	wantToolkitInstallCmd    = "sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades --allow-change-held-packages nvidia-container-toolkit=1.18.2-1 nvidia-container-toolkit-base=1.18.2-1 libnvidia-container-tools=1.18.2-1 libnvidia-container1=1.18.2-1"
	wantHostInstallCmd       = "sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y podman prometheus-node-exporter"
	wantAptMarkHoldCmd       = "sudo apt-mark hold nvidia-container-toolkit nvidia-container-toolkit-base libnvidia-container-tools libnvidia-container1"
	wantAptCachePolicyCmd    = "apt-cache policy nvidia-container-toolkit 2>/dev/null"
	wantMkdirCmd             = "sudo mkdir -p /usr/local/libexec /etc/containers/systemd /var/lib/vllm/hf /etc/vllm /var/lib/prometheus/node-exporter"
	wantNvidiaSmiCmd         = "nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null"
	wantCDIRecordReadCmd     = "sudo cat /etc/vllm/cdi-generated-for 2>/dev/null"
	wantCDIRecordRemoveCmd   = "sudo rm -f /etc/vllm/cdi-generated-for"
	wantNvidiaCtkGenerateCmd = "sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
	wantCDIRefreshCmd        = "sudo systemctl restart nvidia-cdi-refresh.service"
	wantNodeExporterGrepCmd  = "grep '^ARGS=' /etc/default/prometheus-node-exporter 2>/dev/null"

	wantDfVllmCmd       = "df --output=avail -B1 /var/lib/vllm 2>/dev/null | tail -1"
	wantDfContainersCmd = "df --output=avail -B1 /var/lib/containers 2>/dev/null | tail -1"

	wantRunningCmd = "sudo podman inspect --format '{{.State.Running}}' vllm 2>/dev/null"
	wantImageIDCmd = "sudo podman inspect --format '{{.Image}}' vllm 2>/dev/null"

	wantProbeAppliedCmd = "sudo sh -c 'test -e /etc/vllm/applied.json || echo absent'"
	wantCatAppliedCmd   = "sudo cat /etc/vllm/applied.json 2>/dev/null"

	wantIsActiveCmd  = "systemctl is-active vllm.service 2>/dev/null"
	wantIsEnabledCmd = "systemctl is-enabled vllm.service 2>/dev/null"
)

// wantCDIRecordWriteCmd is the record write for driver under the pinned
// toolkit, the only toolkit EnsureHost ever generates with.
func wantCDIRecordWriteCmd(driver string) string {
	return "printf '%s' '" + driver + "|1.18.2-1' | sudo tee /etc/vllm/cdi-generated-for >/dev/null"
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
func wantGPUCheckCmd(image string) string {
	return "sudo podman run --rm --entrypoint true --device nvidia.com/gpu=all '" + image + "'"
}
func wantRepoDigestsCmd(imageID string) string {
	return "sudo podman image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' " + imageID + " 2>/dev/null"
}
func wantArgsCmd() string {
	return "sudo podman inspect --format '" + ArgsInspectFormat + "' vllm 2>/dev/null"
}

// ---- EnsureHost ----

// dpkgReady is dpkg-query's answer on a host EnsureHost has nothing to do
// for: the unpinned packages installed at whatever version, the toolkit
// packages installed at exactly the pinned version and held there.
const dpkgReady = "podman 4.9.3+ds1-1ubuntu0.2 install installed\n" +
	"prometheus-node-exporter 1.7.0-1ubuntu0.3 install installed\n" +
	"nvidia-container-toolkit 1.18.2-1 hold installed\n" +
	"nvidia-container-toolkit-base 1.18.2-1 hold installed\n" +
	"libnvidia-container-tools 1.18.2-1 hold installed\n" +
	"libnvidia-container1 1.18.2-1 hold installed\n"

const (
	wantToolkitInstalledChange = "installed and held nvidia-container-toolkit, nvidia-container-toolkit-base, libnvidia-container-tools, libnvidia-container1 at 1.18.2-1"
	wantHostInstalledChange    = "installed podman, prometheus-node-exporter"
)

// allInstalledOK scripts a fakeRunner where every package is installed, the
// CDI record matches nvidia-smi's driver version, and node-exporter's
// config already matches — the baseline every EnsureHost test starts from
// and overrides pieces of.
func allInstalledOK() *fakeRunner {
	return &fakeRunner{
		out: map[string]string{
			"dpkg-query":                            dpkgReady,
			"nvidia-smi":                            "550.90.07\n",
			"sudo cat /etc/vllm/cdi-generated-for":  "550.90.07|1.18.2-1",
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

func TestEnsureHostInstallsEverythingOnAFreshHost(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantCDIRecordRemoveCmd,
		wantToolkitInstallCmd,
		wantAptMarkHoldCmd,
		wantHostInstallCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRefreshCmd,
		wantCDIRecordWriteCmd("550.90.07"),
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{wantToolkitInstalledChange, wantHostInstalledChange, wantCDIRegeneratedChange}, changed)
}

func TestEnsureHostInstallsAMissingToolkitPackage(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "nvidia-container-toolkit 1.18.2-1 hold installed\n", "", 1)
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantCDIRecordRemoveCmd,
		wantToolkitInstallCmd,
		wantAptMarkHoldCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRefreshCmd,
		wantCDIRecordWriteCmd("550.90.07"),
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{wantToolkitInstalledChange, wantCDIRegeneratedChange}, changed)
}

// Only the missing unpinned package is installed: naming podman too would
// upgrade it, and the toolkit, already pinned and held, is left alone.
func TestEnsureHostInstallsOnlyTheMissingUnpinnedPackage(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "prometheus-node-exporter 1.7.0-1ubuntu0.3 install installed\n", "", 1)
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		"sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y prometheus-node-exporter",
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{"installed prometheus-node-exporter"}, changed)
}

func TestEnsureHostUnpinnedInstallFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "podman 4.9.3+ds1-1ubuntu0.2 install installed\n", "", 1)
	r.fail = map[string]error{"apt-get install": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install podman")
	assert.Contains(t, err.Error(), "fake failure")
	assert.Len(t, r.cmds, 2, "a failed install stops EnsureHost: %v", r.cmds)
}

// TestEnsureHostDuplicateInstalledLineDoesNotMaskAMissingPackage kills the
// "dpkg line-count" mutant: six "installed" lines is coincidentally the
// right count, but two of them are the same package repeated, so
// prometheus-node-exporter was never actually reported.
func TestEnsureHostDuplicateInstalledLineDoesNotMaskAMissingPackage(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "prometheus-node-exporter 1.7.0-1ubuntu0.3", "podman 4.9.3+ds1-1ubuntu0.2", 1)
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{"installed prometheus-node-exporter"}, changed)
}

// The toolkit pin is only met by the exact version, installed and held:
// presence alone is how a host that apt upgraded to a release podman cannot
// read kept passing the check. Podman and node-exporter are already there,
// so the reinstall names neither: apt-get install would upgrade them, and
// restart node-exporter, as a side effect of moving the toolkit.
func TestEnsureHostReinstallsOnlyTheToolkitUnlessExactlyPinnedAndHeld(t *testing.T) {
	cases := map[string]string{
		"newer toolkit":        strings.Replace(dpkgReady, "nvidia-container-toolkit 1.18.2-1", "nvidia-container-toolkit 1.20.1-1", 1),
		"newer base":           strings.Replace(dpkgReady, "nvidia-container-toolkit-base 1.18.2-1", "nvidia-container-toolkit-base 1.20.1-1", 1),
		"newer libnvidia":      strings.Replace(dpkgReady, "libnvidia-container1 1.18.2-1", "libnvidia-container1 1.20.1-1", 1),
		"every package newer":  strings.ReplaceAll(dpkgReady, "1.18.2-1 hold", "1.20.1-1 install"),
		"older tools":          strings.Replace(dpkgReady, "libnvidia-container-tools 1.18.2-1", "libnvidia-container-tools 1.17.8-1", 1),
		"pinned but not held":  strings.Replace(dpkgReady, "libnvidia-container1 1.18.2-1 hold", "libnvidia-container1 1.18.2-1 install", 1),
		"removed, config kept": strings.Replace(dpkgReady, "nvidia-container-toolkit 1.18.2-1 hold installed", "nvidia-container-toolkit 1.18.2-1 hold config-files", 1),
		"version prefix only":  strings.Replace(dpkgReady, "nvidia-container-toolkit 1.18.2-1 ", "nvidia-container-toolkit 1.18.2-10 ", 1),
	}
	for name, dpkg := range cases {
		t.Run(name, func(t *testing.T) {
			r := allInstalledOK()
			r.out["dpkg-query"] = dpkg
			changed, err := EnsureHost(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptMarkHoldCmd, wantMkdirCmd}, r.cmds[:5])
			assert.Equal(t, []string{wantToolkitInstalledChange, wantCDIRegeneratedChange}, changed)
			for _, cmd := range r.cmds {
				if !strings.Contains(cmd, "apt-get install") {
					continue
				}
				for _, arg := range strings.Fields(cmd) {
					assert.NotEqual(t, "podman", arg, "podman must not be reinstalled: %s", cmd)
					assert.NotEqual(t, "prometheus-node-exporter", arg, "node-exporter must not be reinstalled: %s", cmd)
				}
			}
		})
	}
}

// A dpkg-query that could not be run says nothing about what is installed:
// reading it as "nothing is" would reinstall, and so upgrade, packages the
// host already has.
func TestEnsureHostPackageQueryFailureInstallsNothing(t *testing.T) {
	r := allInstalledOK()
	r.fail = map[string]error{"dpkg-query": errFail}

	changed, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.ErrorIs(t, err, errFail)
	assert.Contains(t, err.Error(), "read installed packages")
	assert.Empty(t, changed)
	assert.Equal(t, []string{wantDpkgQueryCmd}, r.cmds)
}

// The unpinned packages are left at whatever version the host has: only
// the toolkit's version is load-bearing.
func TestEnsureHostLeavesUnpinnedPackageVersionsAlone(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "podman 4.9.3+ds1-1ubuntu0.2 install", "podman 5.0.0-1 hold", 1)
	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Zero(t, countContaining(r.cmds, "apt-get"))
}

func TestEnsureHostHoldFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-mark hold": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hold nvidia-container-toolkit packages at 1.18.2-1")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptMarkHoldCmd}, r.cmds)
}

// A repository that carries the toolkit but no longer the pinned release
// fails the install with the same generic apt error; it's named, because
// the fix is a different one from adding the repository.
func TestEnsureHostPinnedVersionMissingFromRepoIsNamed(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = "nvidia-container-toolkit:\n  Installed: (none)\n  Candidate: 1.21.0-1\n  Version table:\n     1.21.0-1 500\n     1.20.1-1 500\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1.18.2-1 is not available")
	assert.Contains(t, err.Error(), "docs/vllm-serving.md#host-prerequisites")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptCachePolicyCmd}, r.cmds)
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
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

func TestEnsureHostEmptyPolicyOutputReturnsDocumentedError(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = ""

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no apt installation candidate")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptCachePolicyCmd}, r.cmds)
}

func TestEnsureHostInstallFailsWithCandidatePresentReturnsGenericError(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = ""
	r.fail = map[string]error{"apt-get install": errFail}
	r.out["apt-cache policy nvidia-container-toolkit"] = "nvidia-container-toolkit:\n  Installed: 1.13.5-1\n  Candidate: 1.20.1-1\n  Version table:\n     1.20.1-1 500\n     1.18.2-1 500\n"

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "docs/vllm-serving.md#host-prerequisites")
	assert.Contains(t, err.Error(), "install host packages")
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptCachePolicyCmd}, r.cmds)
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
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd, wantToolkitInstallCmd, wantAptCachePolicyCmd}, r.cmds)
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
	r.out["sudo cat /etc/vllm/cdi-generated-for"] = "550.54.15|1.18.2-1" // forces regenerate + write

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
	r.out["sudo cat /etc/vllm/cdi-generated-for"] = "999.99.99|1.18.2-1" // the SECOND line's key: a record that matches it must still regenerate

	_, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Contains(t, r.cmds, wantCDIRecordWriteCmd("550.90.07"), "must record the FIRST GPU's driver version, not the second")
}

const wantCDIRegeneratedChange = "regenerated /etc/cdi/nvidia.yaml and /var/run/cdi/nvidia.yaml for driver 550.90.07, toolkit 1.18.2-1"

// The record is keyed on driver AND toolkit: either one changing changes
// what nvidia-ctk writes. A driver-only record, the format before the
// toolkit was pinned, is a toolkit change too — it was written by whatever
// toolkit the host had then, which is how a 1.20.1 spec outlived the pin.
func TestEnsureHostRegeneratesCDIWhenItsKeyDiffers(t *testing.T) {
	for name, record := range map[string]string{
		"driver differs":       "550.54.15|1.18.2-1",
		"toolkit differs":      "550.90.07|1.20.1-1",
		"driver-only record":   "550.90.07",
		"key with extra field": "550.90.07|1.18.2-1|x",
	} {
		t.Run(name, func(t *testing.T) {
			r := allInstalledOK()
			r.out["sudo cat /etc/vllm/cdi-generated-for"] = record

			changed, err := EnsureHost(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, []string{
				wantDpkgQueryCmd,
				wantMkdirCmd,
				wantNvidiaSmiCmd,
				wantCDIRecordReadCmd,
				wantNvidiaCtkGenerateCmd,
				wantCDIRefreshCmd,
				wantCDIRecordWriteCmd("550.90.07"),
				wantNodeExporterGrepCmd,
			}, r.cmds)
			assert.Equal(t, []string{wantCDIRegeneratedChange}, changed)
		})
	}
}

// Installing the pinned toolkit changes its version, so the CDI specs are
// regenerated in the same run even when the host had a record from before.
func TestEnsureHostRegeneratesCDIAfterMovingTheToolkitOntoThePin(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.Replace(dpkgReady, "nvidia-container-toolkit 1.18.2-1", "nvidia-container-toolkit 1.20.1-1", 1)
	r.out["sudo cat /etc/vllm/cdi-generated-for"] = "550.90.07|1.20.1-1"

	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{wantToolkitInstalledChange, wantCDIRegeneratedChange}, changed)
	assert.Less(t, indexOf(r.cmds, wantAptMarkHoldCmd), indexOf(r.cmds, wantNvidiaCtkGenerateCmd), "the spec must be generated by the pinned nvidia-ctk")
}

// The record holds the pin, never the toolkit that was actually installed,
// so a host converged earlier still has a matching record after its toolkit
// moved off the pin and rewrote the specs. Moving it back removes the record
// first, and so regenerates both.
func TestEnsureHostRegeneratesCDIAfterRepinningDespiteAMatchingRecord(t *testing.T) {
	for name, dpkg := range map[string]string{
		"toolkit upgraded past the pin": strings.ReplaceAll(dpkgReady, "1.18.2-1 hold", "1.20.1-1 install"),
		"pinned but not held":           strings.Replace(dpkgReady, "libnvidia-container1 1.18.2-1 hold", "libnvidia-container1 1.18.2-1 install", 1),
	} {
		t.Run(name, func(t *testing.T) {
			r := allInstalledOK()
			r.out["dpkg-query"] = dpkg

			changed, err := EnsureHost(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, []string{
				wantDpkgQueryCmd,
				wantCDIRecordRemoveCmd,
				wantToolkitInstallCmd,
				wantAptMarkHoldCmd,
				wantMkdirCmd,
				wantNvidiaSmiCmd,
				wantCDIRecordReadCmd,
				wantNvidiaCtkGenerateCmd,
				wantCDIRefreshCmd,
				wantCDIRecordWriteCmd("550.90.07"),
				wantNodeExporterGrepCmd,
			}, r.cmds)
			assert.Equal(t, []string{wantToolkitInstalledChange, wantCDIRegeneratedChange}, changed)
		})
	}
}

// A run that re-pins the toolkit and then fails before the specs are
// regenerated must not leave the earlier record behind: the next run finds
// the toolkit pinned and held, and that record would be its only reason to
// think the specs are current.
func TestEnsureHostRegeneratesCDIOnTheRunAfterARepinThatFailedPartWay(t *testing.T) {
	for name, failing := range map[string]string{
		"driver read fails":   "nvidia-smi",
		"spec generate fails": "nvidia-ctk cdi generate",
		"spec refresh fails":  "nvidia-cdi-refresh",
	} {
		t.Run(name, func(t *testing.T) {
			r := allInstalledOK()
			r.out["dpkg-query"] = strings.ReplaceAll(dpkgReady, "1.18.2-1 hold", "1.20.1-1 install")
			r.fail = map[string]error{failing: errFail}

			changed, err := EnsureHost(context.Background(), r)
			require.Error(t, err)
			assert.Equal(t, []string{wantToolkitInstalledChange}, changed)
			assert.Less(t, indexOf(r.cmds, wantCDIRecordRemoveCmd), indexOf(r.cmds, wantToolkitInstallCmd), "the record must be gone before the toolkit moves")
			assert.Equal(t, -1, indexOf(r.cmds, wantCDIRecordWriteCmd("550.90.07")))

			r.out["dpkg-query"] = dpkgReady
			r.fail = nil
			r.cmds = nil

			changed, err = EnsureHost(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, []string{
				wantDpkgQueryCmd,
				wantMkdirCmd,
				wantNvidiaSmiCmd,
				wantCDIRecordReadCmd,
				wantNvidiaCtkGenerateCmd,
				wantCDIRefreshCmd,
				wantCDIRecordWriteCmd("550.90.07"),
				wantNodeExporterGrepCmd,
			}, r.cmds)
			assert.Equal(t, []string{wantCDIRegeneratedChange}, changed)
		})
	}
}

// A record that could not be removed would survive a failed run, so the
// toolkit is not moved while it is still there.
func TestEnsureHostRecordRemovalFailureLeavesTheToolkitAlone(t *testing.T) {
	r := allInstalledOK()
	r.out["dpkg-query"] = strings.ReplaceAll(dpkgReady, "1.18.2-1 hold", "1.20.1-1 install")
	r.fail = map[string]error{"rm -f /etc/vllm/cdi-generated-for": errFail}

	changed, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove CDI spec record")
	assert.Empty(t, changed)
	assert.Equal(t, []string{wantDpkgQueryCmd, wantCDIRecordRemoveCmd}, r.cmds)
}

// /var/run/cdi wins over /etc/cdi for the same device name, so a refresh
// that fails leaves a spec from before the change in force: the record
// must not claim a regeneration that only half happened.
func TestEnsureHostCDIRefreshFailurePropagatesAndSkipsTheRecord(t *testing.T) {
	r := allInstalledOK()
	r.out["sudo cat /etc/vllm/cdi-generated-for"] = "550.90.07|1.20.1-1"
	r.fail = map[string]error{"nvidia-cdi-refresh": errFail}

	_, err := EnsureHost(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh /var/run/cdi/nvidia.yaml")
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRefreshCmd,
	}, r.cmds)
}

func TestEnsureHostRegeneratesCDIWhenRecordMissing(t *testing.T) {
	r := allInstalledOK()
	// Matches only the read ("sudo cat ...cdi-generated-for"), not the
	// write ("... | sudo tee ...cdi-generated-for"): failing both would
	// turn this into an unrelated "record CDI spec key" error.
	r.fail = map[string]error{"sudo cat /etc/vllm/cdi-generated-for": errFail}

	changed, err := EnsureHost(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, []string{
		wantDpkgQueryCmd,
		wantMkdirCmd,
		wantNvidiaSmiCmd,
		wantCDIRecordReadCmd,
		wantNvidiaCtkGenerateCmd,
		wantCDIRefreshCmd,
		wantCDIRecordWriteCmd("550.90.07"),
		wantNodeExporterGrepCmd,
	}, r.cmds)
	assert.Equal(t, []string{wantCDIRegeneratedChange}, changed)
}

// TestEnsureHostNvidiaCtkGenerateFailurePropagates kills the "nvidia-ctk
// failure ignored" mutant: a failed generate must never be followed by a
// write of the driver-version record it didn't actually produce.
func TestEnsureHostNvidiaCtkGenerateFailurePropagates(t *testing.T) {
	r := allInstalledOK()
	r.out["sudo cat /etc/vllm/cdi-generated-for"] = "550.54.15|1.18.2-1" // forces a regenerate attempt
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
	r.out["/etc/default/prometheus-node-exporter"] = "ARGS=\"--collector.textfile.directory=/srv/textfiles\"\n"

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

// Ubuntu's node-exporter already defaults --collector.textfile.directory to
// the directory the drift check writes, and ships ARGS="" — so only an ARGS
// that names some other directory is worth a warning. Warning on every
// ARGS that merely omits the flag fired on every stock host.
func TestEnsureHostNodeExporterTextfileDirectory(t *testing.T) {
	cases := []struct {
		name string
		args string
		err  error
		warn string // the directory the warning must name; "" for no warning
	}{
		{name: "stock empty ARGS", args: "ARGS=\"\"\n"},
		{name: "no ARGS line", err: errFail},
		{name: "other flags only", args: "ARGS=\"--collector.systemd --web.listen-address=:9100\"\n"},
		{name: "explicit default", args: "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter\"\n"},
		{name: "explicit default, trailing slash", args: "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter/\"\n"},
		{name: "explicit default, quoted value", args: "ARGS='--collector.textfile.directory=\"/var/lib/prometheus/node-exporter\"'\n"},
		{name: "explicit default, space separated", args: "ARGS=\"--collector.textfile.directory /var/lib/prometheus/node-exporter\"\n"},
		{name: "other directory", args: "ARGS=\"--collector.textfile.directory=/srv/textfiles\"\n", warn: "/srv/textfiles"},
		{name: "other directory, space separated", args: "ARGS=\"--collector.textfile.directory /srv/textfiles\"\n", warn: "/srv/textfiles"},
		{name: "default named in a longer path", args: "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter-old\"\n", warn: "/var/lib/prometheus/node-exporter-old"},
		{name: "later ARGS line wins", args: "ARGS=\"--collector.textfile.directory=/srv/textfiles\"\nARGS=\"\"\n"},
		{name: "later ARGS line overrides the default", args: "ARGS=\"\"\nARGS=\"--collector.textfile.directory=/srv/textfiles\"\n", warn: "/srv/textfiles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := allInstalledOK()
			r.out["/etc/default/prometheus-node-exporter"] = tc.args
			if tc.err != nil {
				r.fail = map[string]error{"/etc/default/prometheus-node-exporter": tc.err}
			}

			changed, err := EnsureHost(context.Background(), r)
			require.NoError(t, err)
			if tc.warn == "" {
				assert.Empty(t, changed)
				return
			}
			require.Len(t, changed, 1)
			assert.Contains(t, changed[0], "warning: ")
			assert.Contains(t, changed[0], "directory to "+tc.warn+",")
		})
	}
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

// ---- CheckGPU ----

// The check asks podman to resolve the GPU through CDI exactly as the
// Quadlet will, but runs true instead of the server, so it never loads a
// model or holds GPU memory beside the server still serving.
func TestCheckGPUResolvesTheGPUWithoutStartingTheServer(t *testing.T) {
	s := stageSpec()
	r := &fakeRunner{}
	require.NoError(t, CheckGPU(context.Background(), r, s))
	assert.Equal(t, []string{wantGPUCheckCmd(s.Image)}, r.cmds)
}

// Podman's own words decide whether a failed check is a CDI failure:
// the run can also fail for reasons that say nothing about the CDI spec.
func TestCheckGPUFailureIsACDIFailureOnlyWhenPodmanSaysSo(t *testing.T) {
	cases := map[string]struct {
		out     string
		err     error
		wantCDI bool
	}{
		"unresolvable devices on stderr":     {out: "Error: setting up CDI devices: unresolvable CDI devices nvidia.com/gpu=all\n", err: errFail, wantCDI: true},
		"podman output carried in the error": {err: fmt.Errorf("run SSH command: Process exited with status 125, output: Error: setting up CDI devices: unresolvable CDI devices nvidia.com/gpu=all"), wantCDI: true},
		"unparseable spec":                   {out: "Error: failed to parse CDI Spec \"/etc/cdi/nvidia.yaml\"\n", err: errFail, wantCDI: true},
		"ssh dropped":                        {err: fmt.Errorf("dial SSH: connection refused")},
		"container storage broken":           {out: "Error: creating container storage: layer not known\n", err: errFail},
		"runtime failed":                     {out: "Error: OCI runtime error: crun: executable file `true` not found in $PATH\n", err: errFail},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := stageSpec()
			err := CheckGPU(context.Background(), scriptedOutErrRunner{out: tc.out, err: tc.err}, s)
			require.Error(t, err)
			assert.Equal(t, tc.wantCDI, errors.Is(err, ErrCDIUnresolvable))
			assert.ErrorIs(t, err, tc.err, "the underlying error stays in the chain")
			assert.Contains(t, err.Error(), "nvidia.com/gpu=all")
			assert.Contains(t, err.Error(), tc.err.Error())
		})
	}
}

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

// ---- LegacyPresent ----

// pairRunner answers each command with the output AND error scripted for
// the first key it contains: systemctl is-active and is-enabled both exit
// non-zero for most states while still printing them, so the two must be
// scripted together, per command.
type pairRunner struct {
	cmds    []string
	answers map[string]struct {
		out string
		err error
	}
}

func (p *pairRunner) Run(_ context.Context, cmd string) (string, error) {
	p.cmds = append(p.cmds, cmd)
	for k, a := range p.answers {
		if strings.Contains(cmd, k) {
			return a.out, a.err
		}
	}
	return "", errFail
}

func legacyRunner(active, enabled string) *pairRunner {
	return &pairRunner{answers: map[string]struct {
		out string
		err error
	}{
		// Non-zero exits alongside the printed state, as systemctl does for
		// everything but active/enabled: the printed state must still win.
		"is-active vllm.service":  {out: active + "\n", err: errFail},
		"is-enabled vllm.service": {out: enabled + "\n", err: errFail},
	}}
}

func TestLegacyPresentWhenActiveInAnyForm(t *testing.T) {
	for _, status := range []string{"active", "activating", "reloading", "deactivating"} {
		t.Run(status, func(t *testing.T) {
			r := legacyRunner(status, "disabled")
			present, err := LegacyPresent(context.Background(), r)
			require.NoError(t, err)
			assert.True(t, present)
			assert.Equal(t, []string{wantIsActiveCmd}, r.cmds, "an active unit needs no enablement check")
		})
	}
}

// An enabled unit that is not running right now still starts at the next
// boot, beside the Quadlet server, so it is as present as a running one.
func TestLegacyPresentWhenEnabledButNotActive(t *testing.T) {
	for _, active := range []string{"inactive", "failed", "unknown", "maintenance"} {
		for _, enabled := range []string{"enabled", "enabled-runtime"} {
			t.Run(active+"/"+enabled, func(t *testing.T) {
				r := legacyRunner(active, enabled)
				present, err := LegacyPresent(context.Background(), r)
				require.NoError(t, err)
				assert.True(t, present)
				assert.Equal(t, []string{wantIsActiveCmd, wantIsEnabledCmd}, r.cmds)
			})
		}
	}
}

func TestLegacyAbsentWhenNeitherActiveNorEnabled(t *testing.T) {
	notEnabled := []string{"disabled", "static", "masked", "masked-runtime", "indirect", "generated", "transient", "linked", "linked-runtime", "alias", "not-found"}
	for _, enabled := range notEnabled {
		t.Run(enabled, func(t *testing.T) {
			r := legacyRunner("inactive", enabled)
			present, err := LegacyPresent(context.Background(), r)
			require.NoError(t, err)
			assert.False(t, present)
		})
	}
}

// A state this function does not recognise is never read as "absent":
// guessing wrong leaves a legacy server that starts at boot beside the new one.
func TestLegacyPresentUnrecognisedStateIsAnError(t *testing.T) {
	cases := map[string]struct{ active, enabled, want string }{
		"is-active unrecognised":  {active: "some-future-state", enabled: "disabled", want: `"some-future-state"`},
		"is-active empty":         {active: "", enabled: "disabled", want: "check legacy vllm.service"},
		"is-enabled unrecognised": {active: "inactive", enabled: "bad", want: `"bad"`},
		"is-enabled empty":        {active: "inactive", enabled: "", want: "check legacy vllm.service"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			present, err := LegacyPresent(context.Background(), legacyRunner(tc.active, tc.enabled))
			require.Error(t, err)
			assert.False(t, present)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLegacyPresentUnrecognisedWithNilErrorIsAnError(t *testing.T) {
	r := scriptedOutErrRunner{out: "some-future-systemd-state\n", err: nil}
	_, err := LegacyPresent(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "some-future-systemd-state")
}

// ReadLegacy reads both halves even when the unit is active: a rollback
// restores enablement and running state separately.
func TestReadLegacyReadsBothHalves(t *testing.T) {
	cases := map[string]struct {
		active, enabled string
		want            LegacyState
	}{
		"active and enabled":  {"active", "enabled", LegacyState{Active: true, Enabled: true}},
		"active, not enabled": {"active", "disabled", LegacyState{Active: true}},
		"enabled, stopped":    {"inactive", "enabled", LegacyState{Enabled: true}},
		"absent":              {"inactive", "not-found", LegacyState{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := legacyRunner(tc.active, tc.enabled)
			got, err := ReadLegacy(context.Background(), r)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want.Active || tc.want.Enabled, got.Present())
			assert.Equal(t, []string{wantIsActiveCmd, wantIsEnabledCmd}, r.cmds)
		})
	}
}

func TestReadLegacyUnrecognisedStateIsAnError(t *testing.T) {
	for _, tc := range [][2]string{{"weird", "enabled"}, {"active", "bad"}} {
		_, err := ReadLegacy(context.Background(), legacyRunner(tc[0], tc[1]))
		require.Error(t, err, "%v", tc)
		assert.Contains(t, err.Error(), "check legacy vllm.service")
	}
}
