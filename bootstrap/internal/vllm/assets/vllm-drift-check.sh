#!/bin/sh
# Compares the running vllm container against the applied record and writes a
# node-exporter textfile, so drift is visible in Prometheus even though this
# check has no access to serving.yaml or the apply command that wrote them.
set -eu

applied="${VLLM_APPLIED:-/etc/vllm/applied.json}"
dir="${VLLM_TEXTFILE_DIR:-/var/lib/prometheus/node-exporter}"

# No jq on the host; every field this script reads back out is a quoted
# JSON string, so a plain sed capture is enough and avoids the dependency.
field() {
    sed -n 's/.*"'"$1"'": "\([^"]*\)".*/\1/p' "$applied" | head -n 1
}

# Every podman invocation's output lands in one of these before it's read
# back, rather than being captured straight into a shell variable, so a
# podman failure is caught by its own exit status before anything downstream
# tries to hash or compare empty output. All four are scratch files even
# though only some of them are written on any given run; the trap removes
# whichever of them exist. Blank until mktemp runs, so the EXIT trap has
# something quoted to `rm -f` even if this script aborts before that.
running_tmp=
image_id_tmp=
repo_digests_tmp=
args_raw_tmp=
tmp=
trap 'rm -f "$running_tmp" "$image_id_tmp" "$repo_digests_tmp" "$args_raw_tmp" "$tmp"' EXIT
# This runs as root: a `$$`-named temp file's name is predictable, and mktemp
# is what stands between that and a symlink planted at the guessed path.
# `exit N` always runs the EXIT trap too, so the signal handler's only job is
# to turn a signal into an exit dash would otherwise skip that trap for.
trap 'exit 143' TERM INT HUP

running_tmp=$(mktemp "$dir/.vllm-drift-check.running.XXXXXX")
image_id_tmp=$(mktemp "$dir/.vllm-drift-check.image-id.XXXXXX")
repo_digests_tmp=$(mktemp "$dir/.vllm-drift-check.repo-digests.XXXXXX")
args_raw_tmp=$(mktemp "$dir/.vllm-drift-check.args-raw.XXXXXX")
tmp=$(mktemp "$dir/vllm_serving.prom.XXXXXX")

reason=none
commit=
image_digest=
model_revision=
served_name=
args_hash=

if [ ! -f "$applied" ]; then
    reason=no_record
else
    commit=$(field commit)
    image_digest=$(field imageDigest)
    model_revision=$(field modelRevision)
    served_name=$(field servedName)
    args_hash=$(field argsHash)

    if ! podman inspect --format '{{.State.Running}}' vllm >"$running_tmp" 2>/dev/null; then
        reason=not_running
    elif [ "$(cat "$running_tmp")" != "true" ]; then
        reason=not_running
    fi
fi

# applied.json pins the multi-arch index digest (what `podman pull
# <ref>@sha256:<index>` was given), but a running container's own
# .ImageDigest is just its single-arch instance digest — comparing those two
# directly is a permanent false "image" drift on every multi-arch image.
# RepoDigests is the list `podman pull` itself populates from the index
# digest, so checking membership in it is the comparison that actually
# matches what was pulled.
if [ "$reason" = none ]; then
    if ! podman inspect --format '{{.Image}}' vllm >"$image_id_tmp" 2>/dev/null; then
        reason=not_running
    fi
fi

if [ "$reason" = none ]; then
    image_id=$(cat "$image_id_tmp")
    if ! podman image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image_id" >"$repo_digests_tmp" 2>/dev/null; then
        reason=not_running
    else
        image_matches=no
        while IFS= read -r line; do
            case "$line" in
                *"@$image_digest") image_matches=yes ;;
            esac
        done <"$repo_digests_tmp"
        if [ "$image_matches" != yes ]; then
            reason=image
        fi
    fi
fi

if [ "$reason" = none ]; then
    if ! podman inspect --format '{{range $i, $a := .Config.Cmd}}{{if $i}}{{"\x00"}}{{end}}{{$a}}{{end}}' vllm >"$args_raw_tmp" 2>/dev/null; then
        reason=not_running
    else
        # head -c -1 drops podman's own trailing "\n" after the whole
        # --format output; the separator between args is already NUL with
        # none trailing, matching strings.Join(ExecArgs(s), "\x00") exactly.
        current_args_hash=$(head -c -1 "$args_raw_tmp" | sha256sum | cut -d ' ' -f 1)
        if [ "$current_args_hash" != "$args_hash" ]; then
            reason=args
        fi
    fi
fi

{
    for r in none image args not_running no_record; do
        if [ "$r" = "$reason" ]; then
            printf 'vllm_serving_drift{reason="%s"} 1\n' "$r"
        else
            printf 'vllm_serving_drift{reason="%s"} 0\n' "$r"
        fi
    done
    printf 'vllm_serving_info{commit="%s",image_digest="%s",model_revision="%s",served_name="%s"} 1\n' \
        "$commit" "$image_digest" "$model_revision" "$served_name"
    printf 'vllm_serving_drift_check_timestamp_seconds %s\n' "$(date +%s)"
} > "$tmp"
# mktemp creates 0600, but node-exporter reads this as an unprivileged user and
# would silently export none of it.
chmod 0644 "$tmp"
mv "$tmp" "$dir/vllm_serving.prom"
