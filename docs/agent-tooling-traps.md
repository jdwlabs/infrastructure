# Agent tooling traps

Tool behaviours that have misled agents working in this repo. `AGENTS.md`
points here; add a row when a tool's output sends you the wrong way.

RTK's filtered output is not the tool's output — it summarises, truncates, and
prints its own status lines. Every `rtk` row below is that one root cause. Run
anything you intend to act on through `rtk proxy <cmd>` and read the raw result.

| Symptom | Cause | Fix |
|---|---|---|
| A node/object's owning controller is unclear from `kubectl get -o json` | `managedFields` (which manager set which field) is hidden by default | Add `--show-managed-fields` |
| `rtk go build -o <path>` prints `Go build: Success`, exits 1, and writes no binary | RTK's success line doesn't reflect the Go toolchain result; inside a git worktree the real error is `error obtaining VCS status: exit status 128`, which RTK suppresses. `bootstrap/` is a Go module (`talops`) and all work here happens in worktrees, so this is the normal build path — not an edge case | `rtk proxy go build ...` to see the real output; add `-buildvcs=false` when building `bootstrap/` from a worktree |
| `gh pr edit` fails on every PR in this org | `gh` resolves the org through a GraphQL **query** that requires the `read:org` scope, and the active `GITHUB_TOKEN` (`ghp_...`) lacks it — it fails before any mutation is attempted (`the 'login' field requires ... ['read:org']`) | `unset GITHUB_TOKEN` so `gh` falls back to the keyring `gho_` OAuth token, which already carries `read:org`. Fallback if that token is unavailable: `gh api -X PATCH repos/<owner>/<repo>/pulls/<n> --input payload.json` |
| `gh run watch <n>` errors or watches nothing | It takes the run's **databaseId**, not the run number shown in the UI or in a `gh run list` number column | Resolve it first — `gh run list --json databaseId,number,headBranch` — and pass the `databaseId` |
| `.status.containerStatuses[].image` disagrees with the pod spec — a bare `sha256:…` with no repo, or a digest that matches nothing you deployed | That field carries the **config** digest, reported under whichever reference resolved first; `.imageID` carries the repo plus the **manifest** digest. Sampled live: `.image` was `sha256:9700374b…` with no repo while `.imageID` was `docker.io/jdwlabs/ai-sre-relay@sha256:f42b749b…` — two different digests for one running container | Read `.imageID`, never `.status…image`, when verifying which image is running. If the repo names still disagree, compare config/layer digests rather than concluding the wrong image is deployed |
| `curl --cacert <ca>.pem https://host` reports HTTP 000 on Windows | HTTP 000 only means no HTTP response arrived; every transport-level failure reports it, so the status alone cannot tell them apart. Windows curl's Schannel backend **does** honour `--cacert` and fails loudly when the bundle is wrong | Read curl's **exit code**, not the HTTP status: 60 = certificate verification failed, 77 = CA bundle could not be loaded, 7 = connection refused. `-w '%{http_code}'` alone will mislead you |
