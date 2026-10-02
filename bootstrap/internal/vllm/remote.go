package vllm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/hostconverge"
)

// hostPackages are the apt packages EnsureHost requires on the GPU host at
// whatever version apt offers.
var hostPackages = []string{"podman", "prometheus-node-exporter"}

// toolkitVersion is the NVIDIA Container Toolkit release EnsureHost installs
// and holds. Podman 4.9.3, the version Ubuntu 24.04 ships, rejects the whole
// CDI spec that nvidia-ctk 1.19 and later generate (cdiVersion 0.7.0, whose
// additionalGids field its older CDI parser does not know), so every GPU
// container fails with "unresolvable CDI devices". 1.18.x still writes
// cdiVersion 0.5.0. The pin can go once the host's podman vendors a CDI
// library that parses CDI 0.7.0.
const toolkitVersion = "1.18.2-1"

// toolkitPackages are pinned together: they are one release, and apt
// refuses to downgrade nvidia-container-toolkit while its base and library
// packages stay at a newer version.
var toolkitPackages = []string{
	"nvidia-container-toolkit",
	"nvidia-container-toolkit-base",
	"libnvidia-container-tools",
	"libnvidia-container1",
}

// dpkgStatusFormat pairs each reported line with the package it describes:
// checking status lines against a fixed count only proves N status lines
// came back, not that each of the N *requested* packages is one of them
// (dpkg-query can duplicate or omit a line independent of that count).
// The "\n" here must reach dpkg-query as the two literal characters
// (backslash, n) so dpkg-query itself turns it into the newline that
// separates one package's line from the next — a Go-escaped actual newline
// would collapse every package's output onto one line. Status-Want reads
// "hold" for a package apt-mark has held.
const dpkgStatusFormat = `${Package} ${Version} ${db:Status-Want} ${db:Status-Status}\n`

// imageIDPattern is what a genuine podman image ID looks like; ReadLive
// refuses to build a second command around whatever `{{.Image}}` printed
// unless it matches this exactly.
var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// driverVersionPattern is what a genuine nvidia-smi driver_version reading
// looks like. EnsureHost interpolates this value into commands it runs as
// root over SSH (the CDI record write), so it's validated before use the
// same way the image reference is.
var driverVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// cdiRecordPath holds "<driver>|<toolkit>", the pair the CDI specs were
// last generated for.
const cdiRecordPath = "/etc/vllm/cdi-generated-for"

const giB = 1024 * 1024 * 1024

// Staging space requirements, checked on the two filesystems Stage
// actually writes to: the model download (only needed when the marker is
// absent) lands under /var/lib/vllm, while every podman pull — staged or
// not — lands image layers under /var/lib/containers.
const (
	stageRequiredModelBytes = 30 * giB
	stageRequiredImageBytes = 12 * giB
)

// EnsureHost prepares the GPU host for vLLM: it installs the required
// packages (the NVIDIA Container Toolkit at its pinned release), creates
// the directories hostconverge and the Quadlet unit write into, keeps the
// NVIDIA Container Device Interface spec in step with the installed
// driver, and checks (without rewriting) node-exporter's
// textfile collector config. It returns a description of what it changed —
// an empty slice means the host already matched.
//
// Directory creation runs before anything that writes under a directory it
// creates (the CDI record write under /etc/vllm): on a fresh host neither
// exists yet, so writing first would fail permanently.
func EnsureHost(ctx context.Context, r hostconverge.Runner) ([]string, error) {
	var changed []string

	if !packagesInstalled(ctx, r) {
		pinned := make([]string, len(toolkitPackages))
		for i, pkg := range toolkitPackages {
			pinned[i] = pkg + "=" + toolkitVersion
		}
		// --allow-downgrades moves a host that apt already upgraded past
		// the pin back onto it; --allow-change-held-packages lets a change
		// to toolkitVersion move packages this function held at the old one.
		installCmd := "sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades --allow-change-held-packages " +
			strings.Join(hostPackages, " ") + " " + strings.Join(pinned, " ")
		if _, err := r.Run(ctx, installCmd); err != nil {
			return changed, diagnoseInstallFailure(ctx, r, err)
		}
		// Held so an unattended upgrade cannot move the toolkit past the
		// pin between applies.
		if _, err := r.Run(ctx, "sudo apt-mark hold "+strings.Join(toolkitPackages, " ")); err != nil {
			return changed, fmt.Errorf("hold nvidia-container-toolkit packages at %s: %w", toolkitVersion, err)
		}
		changed = append(changed, "installed "+strings.Join(hostPackages, ", ")+"; installed and held "+strings.Join(toolkitPackages, ", ")+" at "+toolkitVersion)
	}

	if _, err := r.Run(ctx, "sudo mkdir -p /usr/local/libexec /etc/containers/systemd /var/lib/vllm/hf /etc/vllm /var/lib/prometheus/node-exporter"); err != nil {
		return changed, fmt.Errorf("create host directories: %w", err)
	}

	driverOut, err := r.Run(ctx, "nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null")
	if err != nil {
		return changed, fmt.Errorf("read GPU driver version: %w", err)
	}
	driverVersion := firstNonEmptyLine(driverOut)
	if driverVersion == "" {
		return changed, fmt.Errorf("nvidia-smi returned no driver version")
	}
	if !driverVersionPattern.MatchString(driverVersion) {
		return changed, fmt.Errorf("nvidia-smi returned a malformed driver version %q", driverVersion)
	}

	// talops records what it last generated the CDI specs for, rather than
	// parsing that spec back out of /etc/cdi/nvidia.yaml: the generated
	// YAML's shape isn't part of nvidia-ctk's contract, so scraping a key
	// out of it is one nvidia-ctk release away from silently never matching
	// again. The key is the driver and the toolkit, because both decide what
	// nvidia-ctk writes: a spec a newer toolkit wrote is unreadable to the
	// host's podman even when the driver never changed.
	cdiKey := driverVersion + "|" + toolkitVersion
	recordOut, recErr := r.Run(ctx, "sudo cat "+cdiRecordPath+" 2>/dev/null")
	if recErr != nil || strings.TrimSpace(recordOut) != cdiKey {
		if _, err := r.Run(ctx, "sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"); err != nil {
			return changed, fmt.Errorf("generate CDI spec: %w", err)
		}
		// The toolkit's own nvidia-cdi-refresh.service writes a second spec
		// to /var/run/cdi, which outranks /etc/cdi for the same device name
		// whenever podman can parse it, so a stale one there shadows the
		// spec just generated; on the host this was found on, it held the
		// same unreadable spec as /etc/cdi. It is re-run rather than
		// disabled: it is also what regenerates that spec after a driver
		// upgrade or a reboot, with the pinned nvidia-ctk.
		if _, err := r.Run(ctx, "sudo systemctl restart nvidia-cdi-refresh.service"); err != nil {
			return changed, fmt.Errorf("refresh /var/run/cdi/nvidia.yaml (nvidia-cdi-refresh.service): %w", err)
		}
		if _, err := r.Run(ctx, "printf '%s' '"+cdiKey+"' | sudo tee "+cdiRecordPath+" >/dev/null"); err != nil {
			return changed, fmt.Errorf("record CDI spec key: %w", err)
		}
		changed = append(changed, "regenerated /etc/cdi/nvidia.yaml and /var/run/cdi/nvidia.yaml for driver "+driverVersion+", toolkit "+toolkitVersion)
	}

	// The package default already sets this on Ubuntu; a mismatch is
	// reported for a human to fix, not rewritten, since /etc/default files
	// are exactly the kind of local edit hostconverge is not meant to own.
	neOut, neErr := r.Run(ctx, "grep '^ARGS=' /etc/default/prometheus-node-exporter 2>/dev/null")
	if neErr != nil || !strings.Contains(neOut, "/var/lib/prometheus/node-exporter") {
		changed = append(changed, "warning: prometheus-node-exporter's --collector.textfile.directory does not match /var/lib/prometheus/node-exporter; check /etc/default/prometheus-node-exporter")
	}

	return changed, nil
}

// diagnoseInstallFailure turns apt's generic failure into the specific,
// common cause when there is one, so the operator is pointed at the manual
// step instead of a bare apt-get failure: no toolkit repository at all, or
// one that no longer carries the pinned release. A policy read that itself
// fails can't tell which case this is, so it's folded into the install
// error rather than misreported as either verdict.
func diagnoseInstallFailure(ctx context.Context, r hostconverge.Runner, installErr error) error {
	policy, policyErr := r.Run(ctx, "apt-cache policy nvidia-container-toolkit 2>/dev/null")
	if policyErr != nil {
		return fmt.Errorf("install host packages: %w (diagnosing candidate: %v)", installErr, policyErr)
	}
	if !strings.Contains(policy, "Candidate:") || strings.Contains(policy, "Candidate: (none)") {
		return fmt.Errorf("nvidia-container-toolkit has no apt installation candidate: add the NVIDIA Container Toolkit apt repository first (talops does not add apt repositories from code) — see docs/vllm-serving.md#host-prerequisites")
	}
	if !policyListsVersion(policy, toolkitVersion) {
		return fmt.Errorf("nvidia-container-toolkit %s is not available from the configured apt repository, and talops pins that release — see docs/vllm-serving.md#host-prerequisites", toolkitVersion)
	}
	return fmt.Errorf("install host packages: %w", installErr)
}

// policyListsVersion reports whether apt-cache policy's version table names
// version as a whole field, so 1.18.2-1 is never satisfied by 1.18.2-10.
func policyListsVersion(policy, version string) bool {
	_, table, ok := strings.Cut(policy, "Version table:")
	if !ok {
		return false
	}
	for _, line := range strings.Split(table, "\n") {
		for _, f := range strings.Fields(line) {
			if f == version {
				return true
			}
		}
	}
	return false
}

// packagesInstalled reports whether every package in hostPackages has an
// "installed" status line naming it, and every package in toolkitPackages
// one that is also exactly toolkitVersion and held — checked per package,
// not by counting lines: a query that duplicates one package's line while
// silently dropping another's would still produce the "right" number of
// installed-looking lines, and a positional line-to-package mapping can't
// tell that apart from every package actually being present.
func packagesInstalled(ctx context.Context, r hostconverge.Runner) bool {
	all := append(append([]string(nil), hostPackages...), toolkitPackages...)
	out, _ := r.Run(ctx, "dpkg-query -W -f='"+dpkgStatusFormat+"' "+strings.Join(all, " ")+" 2>/dev/null")

	type state struct{ version, want string }
	installed := make(map[string]state, len(all))
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && f[3] == "installed" {
			installed[f[0]] = state{version: f[1], want: f[2]}
		}
	}

	for _, pkg := range hostPackages {
		if _, ok := installed[pkg]; !ok {
			return false
		}
	}
	for _, pkg := range toolkitPackages {
		st, ok := installed[pkg]
		if !ok || st.version != toolkitVersion || st.want != "hold" {
			return false
		}
	}
	return true
}

// firstNonEmptyLine returns the first non-blank line of s, trimmed. A
// multi-GPU host prints one driver_version line per GPU; they're always
// identical, so the first one is enough.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// checkFreeSpace fails with an error naming path and the exact shortfall
// when fewer than required bytes are free there.
func checkFreeSpace(ctx context.Context, r hostconverge.Runner, path string, required int64) error {
	avail, err := r.Run(ctx, "df --output=avail -B1 "+path+" 2>/dev/null | tail -1")
	if err != nil {
		return fmt.Errorf("check free space on %s: %w", path, err)
	}
	availBytes, perr := strconv.ParseInt(strings.TrimSpace(avail), 10, 64)
	if perr != nil {
		return fmt.Errorf("parse free space on %s (%q): %w", path, avail, perr)
	}
	if availBytes < required {
		shortfallGiB := (required - availBytes + giB - 1) / giB
		return fmt.Errorf("only %d GiB free on %s, %d GiB short of the %d GiB required to stage the model",
			availBytes/giB, path, shortfallGiB, required/giB)
	}
	return nil
}

// Stage pulls the image and, unless the model revision is already staged,
// downloads it — all while the previous server keeps running, so a slow
// pull or download never causes an outage.
func Stage(ctx context.Context, r hostconverge.Runner, s Spec) error {
	marker := "/var/lib/vllm/hf/.staged-" + s.Model.Revision
	_, markerErr := r.Run(ctx, "sudo test -e "+marker)
	staged := markerErr == nil

	// The model download only happens when unstaged, so that's the only
	// time it needs to be checked; the image pull happens every run and
	// lands on a different filesystem entirely, so it's checked
	// unconditionally against its own path.
	if !staged {
		if err := checkFreeSpace(ctx, r, "/var/lib/vllm", stageRequiredModelBytes); err != nil {
			return err
		}
	}
	if err := checkFreeSpace(ctx, r, "/var/lib/containers", stageRequiredImageBytes); err != nil {
		return err
	}

	// The image itself is validated against spec.go's OCI-reference regex,
	// which excludes shell metacharacters entirely; the quotes here are
	// defence in depth against that regex ever loosening, not a substitute
	// for it.
	if _, err := r.Run(ctx, "sudo podman pull '"+s.Image+"'"); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	if staged {
		return nil
	}

	download := fmt.Sprintf(
		"sudo podman run --rm --entrypoint python3 -v /var/lib/vllm/hf:/root/.cache/huggingface '%s' -c \"from huggingface_hub import snapshot_download; snapshot_download('%s', revision='%s')\"",
		s.Image, s.Model.Repo, s.Model.Revision,
	)
	if _, err := r.Run(ctx, download); err != nil {
		return fmt.Errorf("download model: %w", err)
	}

	// Written only once snapshot_download has actually returned success:
	// the directory it downloads into can exist half-populated after a
	// killed or failed run, so its mere presence must never be read as
	// "already staged" (snapshot_download itself is idempotent, so a
	// re-download of a partial directory is the safe, cheap outcome). A
	// failure here must propagate too — otherwise a marker that silently
	// never got written is read as "unstaged" forever, or worse, a
	// filesystem error here is masked as if staging fully succeeded.
	if _, err := r.Run(ctx, "sudo touch "+marker); err != nil {
		return fmt.Errorf("record staged marker: %w", err)
	}
	return nil
}

// PreflightCDI asks podman to give the staged image the GPU through CDI,
// the same way the Quadlet's AddDevice does, while the previous server keeps
// serving. A CDI spec podman cannot parse otherwise shows up only once the
// swap has already stopped the previous server. The entrypoint is true, so
// the container exits as soon as its devices are set up: it never loads a
// model or takes GPU memory from the server still running.
func PreflightCDI(ctx context.Context, r hostconverge.Runner, s Spec) error {
	if _, err := r.Run(ctx, "sudo podman run --rm --entrypoint true --device nvidia.com/gpu=all '"+s.Image+"'"); err != nil {
		return fmt.Errorf("podman could not give the image the GPU through CDI (--device nvidia.com/gpu=all): %w", err)
	}
	return nil
}

// Live is the state of the running vllm container, read the same way and
// under the same rules as the embedded drift-check script: RepoDigests
// carries the full-image reference list `podman pull` populates from the
// pinned index digest, and ArgsHash is comparable directly against
// ArgsHash(Spec) and against Applied.ArgsHash.
type Live struct {
	Running     bool
	RepoDigests []string
	ArgsHash    string
}

// HasDigest reports whether d — a bare "sha256:..." digest, as recorded in
// Applied.ImageDigest — is one of the running image's RepoDigests. That's
// a suffix match, not equality, because d names the multi-arch index digest
// `podman pull` was given, while each RepoDigests entry is a full
// "repo@sha256:<per-arch instance digest>" reference.
func (l Live) HasDigest(d string) bool {
	suffix := "@" + d
	for _, rd := range l.RepoDigests {
		if strings.HasSuffix(rd, suffix) {
			return true
		}
	}
	return false
}

// ReadLive reads the running vllm container's state. Any podman failure —
// container absent, stopped, or an inspect error — reads as Live{} with no
// error, exactly like the drift script's own not_running case: an SSH
// runner can't tell "container never existed" from "container removed"
// from "podman itself is broken", and none of those is this function's
// failure to report.
func ReadLive(ctx context.Context, r hostconverge.Runner) (Live, error) {
	runningOut, err := r.Run(ctx, "sudo podman inspect --format '{{.State.Running}}' vllm 2>/dev/null")
	if err != nil || strings.TrimSpace(runningOut) != "true" {
		return Live{}, nil
	}

	imageIDOut, err := r.Run(ctx, "sudo podman inspect --format '{{.Image}}' vllm 2>/dev/null")
	if err != nil {
		return Live{}, nil
	}
	imageID := strings.TrimSpace(imageIDOut)
	if !imageIDPattern.MatchString(imageID) {
		return Live{}, nil
	}

	repoDigestsOut, err := r.Run(ctx, "sudo podman image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "+imageID+" 2>/dev/null")
	if err != nil {
		return Live{}, nil
	}
	var repoDigests []string
	for _, line := range strings.Split(strings.TrimRight(repoDigestsOut, "\n"), "\n") {
		if line != "" {
			repoDigests = append(repoDigests, line)
		}
	}

	argsOut, err := r.Run(ctx, "sudo podman inspect --format '"+ArgsInspectFormat+"' vllm 2>/dev/null")
	if err != nil {
		return Live{}, nil
	}
	// Strip ONLY podman's own trailing "\n" after the whole --format
	// output (TrimSuffix, not TrimRight): the NUL-joined args must reach
	// the hash exactly as podman printed them, or this drifts from
	// ArgsHash(Spec) and the drift script the same way it did before.
	argsRaw := strings.TrimSuffix(argsOut, "\n")
	sum := sha256.Sum256([]byte(argsRaw))

	return Live{Running: true, RepoDigests: repoDigests, ArgsHash: hex.EncodeToString(sum[:])}, nil
}

// ReadApplied reads back the record apply last wrote. A missing file is not
// an error: it means nothing has ever been applied. Existence is probed
// separately from the read (the same probe hostconverge.Apply uses) so a
// real read failure — permissions, a truncated file the shell chokes on —
// isn't silently folded into "never applied".
func ReadApplied(ctx context.Context, r hostconverge.Runner) (*Applied, error) {
	probeOut, err := r.Run(ctx, "sudo sh -c 'test -e /etc/vllm/applied.json || echo absent'")
	if err != nil {
		return nil, fmt.Errorf("check applied record: %w", err)
	}
	if strings.Contains(probeOut, "absent") {
		return nil, nil
	}

	out, err := r.Run(ctx, "sudo cat /etc/vllm/applied.json 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("read applied record: %w", err)
	}

	var a Applied
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return nil, fmt.Errorf("parse applied record: %w", err)
	}
	return &a, nil
}

// LegacyPresent reports whether the pre-Quadlet vllm.service unit still has
// a claim on the host: running or winding up or down (active, activating,
// reloading, deactivating), or enabled to start at the next boot. An
// enabled-but-stopped legacy unit counts, because left alone it starts at
// boot beside the Quadlet server and fights it for the port and the GPU.
//
// systemctl is-active and is-enabled exit non-zero for most states, so the
// printed state is read first, regardless of the runner's error. A state
// this function does not recognise is an error, never "absent": guessing
// wrong means never migrating away from a unit that still serves.
func LegacyPresent(ctx context.Context, r hostconverge.Runner) (bool, error) {
	active, err := legacyActive(ctx, r)
	if err != nil || active {
		return active, err
	}
	return legacyEnabled(ctx, r)
}

// LegacyState is the legacy unit's running and boot-time state, read apart
// because a rollback has to put back exactly the pair it found: a unit that
// was enabled but stopped served nothing, and starting it on rollback would
// report a server as restored that never ran before the apply.
type LegacyState struct {
	Active  bool
	Enabled bool
}

// Present is LegacyPresent's answer for this state.
func (l LegacyState) Present() bool { return l.Active || l.Enabled }

// ReadLegacy reads both halves of the legacy unit's state, under the same
// recognition rules as LegacyPresent.
func ReadLegacy(ctx context.Context, r hostconverge.Runner) (LegacyState, error) {
	active, err := legacyActive(ctx, r)
	if err != nil {
		return LegacyState{}, err
	}
	enabled, err := legacyEnabled(ctx, r)
	if err != nil {
		return LegacyState{}, err
	}
	return LegacyState{Active: active, Enabled: enabled}, nil
}

func legacyActive(ctx context.Context, r hostconverge.Runner) (bool, error) {
	out, err := r.Run(ctx, "systemctl is-active vllm.service 2>/dev/null")
	switch status := strings.TrimSpace(out); status {
	case "active", "activating", "reloading", "deactivating":
		return true, nil
	case "inactive", "failed", "unknown", "maintenance":
		return false, nil
	default:
		return false, legacyStateError("is-active", status, err)
	}
}

func legacyEnabled(ctx context.Context, r hostconverge.Runner) (bool, error) {
	out, err := r.Run(ctx, "systemctl is-enabled vllm.service 2>/dev/null")
	switch state := strings.TrimSpace(out); state {
	case "enabled", "enabled-runtime":
		return true, nil
	case "disabled", "static", "masked", "masked-runtime", "indirect", "generated",
		"transient", "linked", "linked-runtime", "alias", "not-found":
		return false, nil
	default:
		return false, legacyStateError("is-enabled", state, err)
	}
}

func legacyStateError(query, state string, err error) error {
	if state == "" && err != nil {
		return fmt.Errorf("check legacy vllm.service (%s): %w", query, err)
	}
	return fmt.Errorf("check legacy vllm.service (%s): unrecognised state %q", query, state)
}
