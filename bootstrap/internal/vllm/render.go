package vllm

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/hostconverge"
)

//go:embed assets/*
var assets embed.FS

// ArgsInspectFormat is the podman inspect --format string that prints a
// running container's argv NUL-separated (the separator sits BETWEEN
// elements, not after the last one), so it hashes to the same bytes as
// strings.Join(ExecArgs(s), "\x00") once podman's own trailing "\n" after
// the whole --format output is stripped.
const ArgsInspectFormat = `{{range $i, $a := .Config.Cmd}}{{if $i}}{{"\x00"}}{{end}}{{$a}}{{end}}`

const quadletTemplate = "# Rendered by talops from inference/vllm/serving.yaml. Edit that file and run\n" +
	"# `talops vllm apply`; changes made here are reported as drift.\n" +
	"[Unit]\n" +
	"Description=vLLM OpenAI-compatible server\n" +
	"After=network-online.target\n" +
	"Wants=network-online.target\n" +
	"\n" +
	"[Container]\n" +
	"Image=%s\n" +
	"ContainerName=vllm\n" +
	"AddDevice=nvidia.com/gpu=all\n" +
	"Volume=/var/lib/vllm/hf:/root/.cache/huggingface\n" +
	"PublishPort=%[2]d:%[2]d\n" +
	"Environment=HF_HUB_OFFLINE=1\n" +
	"Exec=%s\n" +
	"\n" +
	"[Service]\n" +
	"Restart=always\n" +
	"TimeoutStartSec=900\n" +
	"\n" +
	"[Install]\n" +
	"WantedBy=multi-user.target\n"

// Quadlet renders the Podman Quadlet unit that runs the vLLM server.
func Quadlet(s Spec) string {
	return fmt.Sprintf(quadletTemplate, s.Image, s.Port, strings.Join(ExecArgs(s), " "))
}

// ExecArgs is the full `vllm serve` argv after the image: the model repo is
// the vllm-openai entrypoint's first positional, followed by the flags the
// spec forbids overriding directly, then the spec's own args.
//
// servedName leads the --served-model-name values because position is what
// vLLM v0.24.0 gives meaning to. The flag is a list (nargs="+",
// engine/arg_utils.py:223), and init_app_state makes one BaseModelPath per
// value, all rooted at the model repo (entrypoints/openai/api_server.py:328-340).
// A request naming any of them is accepted (is_base_model,
// entrypoints/openai/models/serving.py:50-51, via _check_model,
// entrypoints/serve/engine/serving.py:44,68-73); /v1/models lists every one
// in this order, each with that same root (models/serving.py:64-76); and a
// response's "model" field is always the first, whichever name was asked
// for (model_name, models/serving.py:144-147, used at
// chat_completion/serving.py:283 and completion/serving.py:219).
func ExecArgs(s Spec) []string {
	args := []string{
		s.Model.Repo,
		"--revision", s.Model.Revision,
		"--served-model-name", s.ServedName,
	}
	args = append(args, s.ServedAliases...)
	args = append(args,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(s.Port),
	)
	return append(args, s.Args...)
}

// ArgsHash is the hex sha256 of ExecArgs joined by NUL, with no trailing NUL.
// The embedded drift script recomputes this same hash from the running
// container's argv via ArgsInspectFormat, so a test running that pipeline
// against a known ExecArgs is the proof the two can't drift apart.
func ArgsHash(s Spec) string {
	sum := sha256.Sum256([]byte(strings.Join(ExecArgs(s), "\x00")))
	return hex.EncodeToString(sum[:])
}

// Applied is what apply last wrote to the host. The drift check reads it
// back from disk with no access to serving.yaml or ArgsHash's inputs, so
// every value it needs to compare against is recorded here, already computed.
type Applied struct {
	Commit        string `json:"commit"`
	Image         string `json:"image"`
	ImageDigest   string `json:"imageDigest"`
	ModelRepo     string `json:"modelRepo"`
	ModelRevision string `json:"modelRevision"`
	ServedName    string `json:"servedName"`
	// omitempty keeps an unaliased record byte-for-byte the shape written
	// before aliases existed.
	ServedAliases []string  `json:"servedAliases,omitempty"`
	ArgsHash      string    `json:"argsHash"`
	AppliedAt     time.Time `json:"appliedAt"`
	AppliedBy     string    `json:"appliedBy"`
}

// NewApplied builds the record apply writes to /etc/vllm/applied.json after
// a successful convergence.
func NewApplied(s Spec, commit, by string, at time.Time) Applied {
	return Applied{
		Commit:        commit,
		Image:         s.Image,
		ImageDigest:   s.ImageDigest(),
		ModelRepo:     s.Model.Repo,
		ModelRevision: s.Model.Revision,
		ServedName:    s.ServedName,
		ServedAliases: s.ServedAliases,
		ArgsHash:      ArgsHash(s),
		AppliedAt:     at,
		AppliedBy:     by,
	}
}

// JSON renders it indented, with a trailing newline. The embedded drift
// script's sed patterns key on the `"field": "value"` shape MarshalIndent
// produces, so the format here is load-bearing, not cosmetic.
func (a Applied) JSON() []byte {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		// Applied is all strings and a time.Time; nothing in it can fail to marshal.
		panic(err)
	}
	return append(b, '\n')
}

// DriftFiles are the drift-check script and its systemd service and timer,
// embedded into the binary so talops ships them without a separate release
// artifact to keep in sync.
func DriftFiles() []hostconverge.File {
	return []hostconverge.File{
		{Path: "/usr/local/libexec/vllm-drift-check", Content: mustAsset("vllm-drift-check.sh"), Mode: 0o755},
		{Path: "/etc/systemd/system/vllm-drift-check.service", Content: mustAsset("vllm-drift-check.service")},
		{Path: "/etc/systemd/system/vllm-drift-check.timer", Content: mustAsset("vllm-drift-check.timer")},
	}
}

func mustAsset(name string) []byte {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		// Embedded at build time: a missing asset is a build-time bug, not a runtime one.
		panic(err)
	}
	return b
}
