# devbox2 — Lifeboat VM Provisioning and the pve5 Memory Reclaim

Status: **§5 step 1 applied — devbox2 is live on pve1 as VMID 112
(192.168.1.57). The pve5 memory reclaim is not yet applied.** Capacity figures
captured live 2026-09-01 via `pvesh` and `kubectl`; re-verify before acting if
reading this more than a few weeks later.

**Read §5's "Standing state drift" before running any `terraform apply` from
this document.** An untargeted apply no longer destroys devbox — the
`lifecycle` guards in `terraform/` closed that — but it still replaces
cloud-init snippets, which arms a cloud-init re-run on the next boot of every
guest whose snippet it touches, including the production load balancer.

Companion runbook: `scenarios/host-restart-coordination.md`.
Related: `docs/dev-vm-provisioning.md` (devbox itself),
`scenarios/dev-vm-migrate.md` (still blocked, see §7),
`docs/ram-expansion-decision.md` (superseded in part, see §8).

## 1. The problem

devbox is the only interactive workstation in the homelab and it runs on pve5.
pve5 is also the host most in need of a reboot, and the one carrying the most
memory pressure. Rebooting it today means having nowhere to work from while it
is down — no shell, no `kubectl`, no agent sessions, no way to drive the
restart itself.

Two separate problems sit underneath that, and the fix for one is the
precondition for the fix for the other.

## 2. Capacity findings (2026-09-01)

### Hosts

| Host | CPU | Threads | RAM physical | DIMM slots | VM RAM allocated | Unallocated |
|---|---|---|---|---|---|---|
| pve1 | Ryzen 7 8845HS | 16 | 28.2 GB | 2×16 GB DDR5-5600, 0 free, max 64 GB | 23 GB | 5.2 GB |
| pve2 | Ryzen 5 3550H | 8 | 12.6 GB | 1×16 GB DDR4-2667, **1 free**, max 32 GB | 10 GB | 2.6 GB |
| pve3 | Ryzen 5 3550H | 8 | 12.6 GB | 1×16 GB DDR4-2667, **1 free**, max 32 GB | 10 GB | 2.6 GB |
| pve4 | Ryzen 5 3550H | 8 | 12.6 GB | 1×16 GB DDR4-2667, **1 free**, max 32 GB | 10 GB | 2.6 GB |
| pve5 | Ryzen 9 9950X | 32 | 123.5 GB | 4×32 GB DDR5-4800, 0 free, maxed | **128 GB** | **−4.5 GB** |

Fleet totals: 72 threads, 189.5 GB. Allocated vCPU is 68 of 72, but measured
host CPU utilisation runs 4–19%. **CPU is not the binding constraint; memory
is.** That asymmetry is why every decision below trades CPU freely and memory
carefully.

### pve5 is over-allocated

devbox (32 GB) + talos-worker-05 (64 GB) + vllm-inference (32 GB) = 128 GB
assigned against 123.5 GB of physical RAM. All three guests are configured
`balloon: 0`, so the hypervisor has no reclaim path — these are dedicated
allocations, not a soft overcommit that resolves itself under pressure.

```
root@pve5:~# free -g
               total        used        free      shared  buff/cache   available
Mem:             123         117           2           0           3           5
root@pve5:~# cat /sys/kernel/mm/ksm/pages_sharing
4610779
```

Two gigabytes free. The host is solvent only because KSM is deduplicating
~17.6 GB of identical pages across the three guests. That is a real mechanism,
not an accounting artifact, but it is also load-dependent and not something to
rely on: if the guests' memory contents diverge, the saving shrinks and the
host has nowhere to go.

### devbox is provisioned four times larger than it runs

```
dev-admin@devbox:~$ free -g
               total        used        free      shared  buff/cache   available
Mem:              31           4           3           0          23          26
```

Four gigabytes resident. The 23 GB in `buff/cache` is page cache — useful for
the Nx/pnpm build workloads devbox was sized for, but reclaimable on demand and
not a reason to hold 32 GB of dedicated hypervisor memory.

## 3. Design — devbox2

A deliberately small always-on VM on pve1, sized to be a lifeboat rather than a
second daily driver. Its job is to remain reachable when pve5 is down, long
enough to drive a restart and keep working in a degraded but functional state.

| Field | Value | Rationale |
|---|---|---|
| VMID | 112 | next free after devbox's 111 |
| Host | pve1 | the only host with unallocated memory, and a different physical machine from devbox |
| Cores | 2 | pve1 has 4 unallocated threads of 16; CPU is not scarce here |
| Memory | 2048 MB, `balloon: 0` | takes pve1 to 25 GB allocated of 28.2 GB, leaving 3.2 GB hypervisor reserve; matches the fleet's dedicated-memory convention |
| Disk | 32 GB on `truenas-vmdisks` | NFS-backed, not pinned to pve1's local storage |
| Address | 192.168.1.57/24, gateway 192.168.1.254 | below the .64 DHCP pool floor, adjacent to devbox's .56 |
| Admin user | `dev-admin`, existing SSH key | existing Remote-SSH and tailnet habits carry over unchanged |
| Snippet datastore | `local` on pve1 | same pattern haproxy-1 already uses on this host; see §7 |
| `on_boot` | true | returns unattended after a pve1 reboot |
| Tags | `dev;lifeboat` | |

`terraform/devbox2-node.tf` reuses `dev_vm_cloud_image_url` and
`dev_vm_cloud_image_checksum` rather than defining its own — one Ubuntu
release to track, one checksum to rotate, at the cost that an image rotation
touches both devbox and devbox2 in the same `apply`. That is a real
trade-off for two machines whose whole point is not sharing fate, accepted
here because the alternative (a second set of image variables tracking the
same upstream release) is duplication without a corresponding safety gain —
an image rotation is reviewed either way, and it is the pve1/pve5 physical
separation and the independent VMIDs, not the image source, that keep a
pve5 outage from taking devbox2 down too.

devbox2 is deliberately outside the nightly `vzdump` job (§6): that job's
`vmid` is `111` only, not `all`, so devbox2 gets no backup of its own. It
doesn't need one — everything about it is reproducible from
`terraform/devbox2-node.tf` plus the post-boot sequence below, not
accumulated state worth preserving.

Cloud-init is day-0 only, matching the convention `docs/dev-vm-provisioning.md`
§5.2 established for devbox and the haproxy design before it: OS, user, base
packages (`qemu-guest-agent`, `git`, `curl`, `ca-certificates`), and
tailscale. tailscale is installed but deliberately not authenticated —
`tailscale up` needs an interactive login or an auth key, and baking an
auth key into the cloud-init snippet would leave it in plaintext in
`devbox2-cloud-init.yaml` on the `local` snippet datastore (the table
above), readable by anything with node access to pve1. Everything else —
kubectl, talosctl, the chezmoi-managed dotfiles, the Claude Code CLI, and
SSH keys — is a documented post-boot sequence (below), not committed
automation, the same boundary devbox's own §5.3 draws. Deliberately no
Docker and no Node toolchain either way — build capability is what devbox
is for, and adding it here would grow the VM past the memory budget that
makes it fit on pve1 at all.

### Post-boot bootstrap (manual, one-time)

SSH in once cloud-init completes (`ssh dev-admin@192.168.1.57`), then:

1. Authenticate tailscale — the one piece cloud-init deliberately left
   undone:

   ```sh
   sudo tailscale up
   ```

   Interactive login, or an auth key entered here rather than shipped in the
   snippet.

2. Pull the dotfiles via chezmoi, personal role, **`installDevTooling` left
   at its default `false`**:

   ```sh
   sh -c "$(curl -fsLS get.chezmoi.io)" -- init --apply jdwillmsen
   ```

   Unlike devbox's bootstrap (`docs/dev-vm-provisioning.md` §5.3), devbox2
   never answers `installDevTooling` `true`. That flag installs Docker,
   Node/pnpm, Rust, and JDK 21 as one bundle together with kubectl and
   talosctl (`home/run_once_49-install-dev-tools.sh.tmpl` in the dotfiles
   repo) — there's no way to opt into the bundle's kubectl/talosctl without
   also taking Docker and Node, and doing that would be exactly the
   memory-budget mistake "Why 2 GB and not more" (below) argues against.
   kubectl and talosctl are installed directly instead, outside that bundle.

3. Install kubectl and talosctl directly — the fleet's pinned versions, just
   run without the flag that would also pull in Docker and Node. `mkdir -p`
   the keyring directory first: devbox2's cloud-init only ever creates
   `/usr/share/keyrings` (for tailscale), never `/etc/apt/keyrings`, and
   devbox2 skips the Docker/gh installs that create the latter as a side
   effect on devbox — so on a fresh devbox2 `gpg --dearmor -o` into it fails
   with "No such file or directory" (`gpg -o` does not create missing parent
   directories; verified against a real path). talosctl's target,
   `/usr/local/bin`, is a stock FHS directory and needs no such step:

   ```sh
   sudo mkdir -p -m 755 /etc/apt/keyrings
   curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.36/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubectl.gpg
   echo "deb [signed-by=/etc/apt/keyrings/kubectl.gpg] https://pkgs.k8s.io/core:/stable:/v1.36/deb/ /" | sudo tee /etc/apt/sources.list.d/kubernetes.list
   sudo apt-get update && sudo apt-get install -y kubectl

   curl -fsSL -o /usr/local/bin/talosctl https://github.com/siderolabs/talos/releases/download/vX.Y.Z/talosctl-linux-amd64
   chmod +x /usr/local/bin/talosctl
   ```

   Don't hardcode the talosctl version above — pin it to whatever the
   cluster is actually running, checked live rather than assumed stale:

   ```sh
   kubectl get nodes -o wide   # OS-IMAGE column, e.g. Talos (v1.13.4)
   ```

   Client/server skew is tolerated, but there's no reason to run a client
   that's already behind the cluster on day one.

4. Copy the existing `kubeconfig` and `talosconfig` over — devbox2 needs the
   same cluster credentials devbox already holds, not a freshly generated
   set.

5. Install the Claude Code CLI. **The dotfiles repo does not do this.**
   Checking `~/.local/share/chezmoi` directly: `agentClis` in
   `home/.chezmoidata.yaml` lists exactly `no-mistakes` and `gnhf`, no
   `claude` entry, and the script that installs that list is
   `home/run_onchange_43-install-agent-clis.sh.tmpl` — `run_onchange_`, not
   `run_once_`. On devbox, `claude` is a symlink into
   `~/.local/share/claude/versions/`, which nothing in this dotfiles repo
   writes; it got there out-of-band. **Confirm the current official install
   method before relying on this step during an incident** — this document
   does not have a verified command for it on this machine, and guessing
   one here would be worse than saying so.

6. Sync SSH keys from the existing Remote-SSH/tailnet setup so outbound
   `git`/`gh` and inbound SSH behave the same as on devbox. This key also
   has to be authorized as `root@pve1` through `root@pve5` — every `qm` and
   `pvecm` command in this document and
   `scenarios/host-restart-coordination.md` depends on it, and nothing
   above provisions that authorization automatically.

7. Install Terraform >= 1.16.0. **Don't trust the dotfiles bootstrap for
   this one.** `run_once_46-install-cloud-clis.sh` installs Terraform
   ungated — it ran in step 2 regardless of `installDevTooling` — but its
   `TERRAFORM_FALLBACK_VERSION` is `1.15.8`, below `terraform/providers.tf`'s
   `required_version = ">= 1.16.0"`. That fallback is what actually lands if
   the GitHub releases API lookup the script prefers fails or is
   rate-limited, the same trap that had to be worked around getting devbox
   itself onto a qualifying version. Verify what's actually present and
   replace it if it's too old:

   ```sh
   terraform version
   # if < 1.16.0: download a qualifying release from
   # https://releases.hashicorp.com/terraform/ and install it over the
   # existing binary, checksum-verified, same as run_once_46 does
   ```

8. Install `sops`. It lives in `home/run_once_49-install-dev-tools.sh.tmpl`,
   the bundle behind `installDevTooling`, which step 2 deliberately left
   `false` — so it never arrived. Install it directly, the same pattern
   step 3 used for kubectl/talosctl rather than opting into the whole
   bundle it would otherwise come with:

   ```sh
   curl -fsSL -o /tmp/sops.deb https://github.com/getsops/sops/releases/download/vX.Y.Z/sops_X.Y.Z_amd64.deb
   sudo apt-get install -y /tmp/sops.deb
   ```

   Pin `vX.Y.Z` to whatever `home/run_once_49-install-dev-tools.sh.tmpl`
   currently pins for devbox, checked live rather than assumed stale.

9. Get an age identity and become an authorized vault device
   (`docs/secrets.md`). This repo's Terraform state backend credentials and
   `terraform.tfvars` are SOPS-encrypted; decrypting them with an
   unauthorized key fails closed no matter how correctly step 8 installed
   the tool. Either copy devbox's existing age key across (treating devbox2
   as the same operator identity) or generate a new one and have an
   already-authorized device re-key the vault for it:

   ```sh
   age-keygen -o ~/.config/sops/age/keys.txt
   # from an already-authorized device: talops secrets add-device <devbox2's printed public key>
   #   then: git commit -am "chore(secrets): authorize devbox2" && git push
   # on devbox2, once that's pushed: git pull
   ```

10. Clone this repository and decrypt the Terraform inputs devbox2 needs —
    `terraform.tfvars` (Proxmox credentials) and `backend-credentials.enc.yaml`
    (the MinIO/S3 keys the state backend authenticates with) — via `sops -d`,
    per `docs/secrets.md`. The public MinIO CA certificate is committed in
    plaintext as `terraform/minio-ca.crt` and is already in the clone;
    `terraform init` reads it from `custom_ca_bundle = "minio-ca.crt"` in
    `providers.tf`. `backend-tls.enc.yaml` holds only the private halves
    (server cert/key and CA private key) and is not needed for `terraform init`
    to reach the backend — only the backend *credentials* and tfvars are
    required to authenticate and plan.

§6's pre-resize verification list — tailnet + `.57` reachable, `kubectl get
nodes`, `talosctl`, and `terraform init && terraform plan` run *from
devbox2* — is what confirms this sequence actually landed, and it has to
pass before §5 gets anywhere near stopping devbox.

Implementation follows the repository's existing one-file-per-role Terraform
layout (`dev-vm-node.tf`, `haproxy-node.tf`, `gpu-node.tf`) with a new
`terraform/devbox2-node.tf` and its own `devbox2_*` variables. No shared module
is introduced: the repository has none today, and two dev VMs do not justify
the indirection.

### Why 2 GB and not more

pve1 has 5.2 GB unallocated. A larger lifeboat would require reclaiming memory
from `talos-worker-01`, which at 22 GB and 41% memory requests genuinely has
slack. That was considered and rejected for this change: it widens the blast
radius from "add a VM" to "resize a Kubernetes node", and the smaller VM is
sufficient for the stated job. A single Claude Code session is roughly 600 MB
resident, so 2 GB covers a shell, the tooling, and one working agent session.
It will feel cramped with several concurrent sessions; that is the accepted
trade.

## 4. Design — the pve5 reclaim

`dev_vm_memory` moves from 32768 to 16384. pve5 goes from 128 GB allocated on
123.5 GB physical to 112 GB, restoring ~11.5 GB of genuine reserve and removing
the dependency on KSM for solvency. devbox retains 12 GB of headroom above its
4 GB working set for page cache.

Because `balloon: 0` means dedicated memory, this requires a full stop and
start of the VM. A reboot from inside the guest will not re-read the allocation.

## 5. Ordering

### Standing state drift — read this before any apply

devbox2 has since been provisioned, and the drift this section warned about
has been narrowed by the `lifecycle` guards now carried in `terraform/`. What
follows is the state of it, not history: read `terraform/README.md` for why
the guards are shaped the way they are.

Planned against live state, 2026-09-05:

```
Plan: 6 to add, 2 to change, 6 to destroy    # before the guards
Plan: 2 to add, 6 to change, 2 to destroy    # with the guards
```

The six destroys included devbox itself:

```
  # proxmox_virtual_environment_vm.dev_vm must be replaced
      ~ user_data_file_id = "local:snippets/devbox-cloud-init.yaml" -> (known after apply) # forces replacement
```

The cause was line endings, not configuration. The snippet recorded in state
was uploaded with CRLF and the template renders LF today, so `source_raw.data`
differs on every line while the content is byte-identical after
normalisation. That attribute forces replacement of the snippet file, which
makes `user_data_file_id` unknown, which forces replacement of the VM — a
destroy and recreate of devbox, losing its disk. `.gitattributes` pins
`*.tftpl` to LF so a CRLF checkout cannot reintroduce the divergence, but it
does not retroactively fix state written before it existed.

`ignore_changes` on `initialization[0].user_data_file_id` now breaks that
cascade for devbox, devbox2 and haproxy-1, and `overwrite = false` plus
`ignore_changes` on the image checksums removes the three `download_file`
replacements. What remains in the destroy column is the two snippet files
themselves, and neither is drift to be suppressed: devbox's is the CRLF
record, and haproxy-1's is the real, still-unapplied LAN split-horizon DNS
content.

Two consequences for the ordering below:

- **Anything touching devbox must still be `-target`ed.** Replacing a snippet
  gives its guest a new cloud-init `instance-id`, so cloud-init re-runs its
  per-instance modules at that guest's next boot. An untargeted apply would
  arm that on the production load balancer as a side effect of resizing a
  dev VM.
- **Clear devbox's snippet record before the resize**, so the resize reboot
  is not also a cloud-init re-run. The repair is a state correction, not a
  file replacement — `terraform/README.md` has the procedure and the
  alternatives that were rejected. Treat "`dev_vm` shows an in-place `memory`
  change and nothing else" as something to confirm against a fresh plan, not
  to assume.

### The sequence

The ordering is not incidental — the reclaim stops the machine the operator
works from, so the lifeboat has to exist and be proven able to actually do
the job it exists for — including run Terraform — before that happens. The
operator should discover a broken lifeboat while devbox is still up to fix
it from, not after.

1. Apply devbox2 on pve1, scoped to its own three resources so the standing
   drift above stays untouched. No downtime for anything existing:
   ```sh
   terraform plan \
     -target=proxmox_virtual_environment_download_file.devbox2_cloud_image \
     -target=proxmox_virtual_environment_file.devbox2_cloud_init \
     -target=proxmox_virtual_environment_vm.devbox2 \
     -out=tfplan
   terraform show tfplan   # confirm: 3 to add, 0 to change, 0 to destroy
   terraform apply tfplan
   ```
   Read the plan before applying it. Three creates and nothing else is the
   whole acceptance criterion for this step.
2. Bootstrap devbox2 (post-boot sequence above) and run §6's pre-resize
   verification, ending with `terraform init && terraform plan` executed
   *from devbox2*. Stop here if any of it doesn't come back clean — nothing
   has been touched on devbox or pve5 yet.
3. Clear devbox's snippet record (above) as a separate reviewed change, and
   confirm by fresh plan that `dev_vm_cloud_init` no longer appears and
   `dev_vm` is down to an in-place `memory` change. The `lifecycle` guards
   mean the resize no longer destroys the VM if this is skipped, but skipping
   it spends a cloud-init re-run on the resize reboot for nothing.
4. Commit and push any in-progress work on devbox — the next step powers it
   off. From devbox2, apply the `dev_vm_memory` change scoped to the VM
   (`-target=proxmox_virtual_environment_vm.dev_vm`, expecting `0 to add,
   2 to change, 0 to destroy`), then force a full
   stop/start of devbox (a reboot from inside the guest will not re-read a
   `balloon: 0` allocation change; `qm shutdown` rather than `qm stop`
   because this is a hard power-off of the operator's own workstation and the
   guest agent is available to shut down cleanly):
   ```sh
   ssh root@pve5 'qm shutdown 111'
   ssh root@pve5 'qm start 111'
   ```
5. Verify pve5 and devbox.

## 6. Verification

**Before touching devbox — all of this has to pass first, per §5 step 2:**

- `terraform fmt -check` and `terraform validate` clean; the `-target`ed
  devbox2 plan (§5 step 1) reviewed by a human and showing three creates and
  nothing else before any apply.
- devbox2 answers on both the tailnet and 192.168.1.57.
- From devbox2: `kubectl get nodes` returns 8 Ready; `talosctl` reaches the
  control planes.
- From devbox2: `terraform init` succeeds against the MinIO state backend and
  `terraform plan` runs to completion. Expect it to reproduce the standing
  drift described in §5, not a clean `dev_vm_memory`-only diff — what this
  step verifies is that devbox2 can reach the backend and decrypt the vault
  at all, which is a different claim from the binaries having installed. The
  plan is clean enough to proceed on only after §5 step 3 clears the record.
- `claude` is not part of this gate: the lifeboat's job (`ssh`, `kubectl`,
  `talosctl`, `terraform`) doesn't need it, and there's no verified install
  method for it yet (post-boot step 5) — it stays a best-effort follow-up,
  not a blocker.

**After the resize:**

- `free -g` on pve5 reports at least 11 GB free with all three guests
  running; devbox SSH is back; the cluster still shows 8 nodes Ready.
- The 02:00 `vzdump` of VM 111 succeeds on the following night. Seven healthy
  dailies exist today on `truenas-backup`, averaging ~19.8 GB.

## 7. What this does not solve

**vllm-inference cannot survive a pve5 reboot.** It holds an RTX 5090 through
PCI passthrough (`hostpci0 mapping=gpu-rtx5090`) and its disk is on pve5's
local-lvm. It is unmigratable by construction. Every pve5 restart takes the
local LLM tier down, and no amount of memory rebalancing changes that — it
would take a second GPU.

**devbox still cannot migrate anywhere.** `scenarios/dev-vm-migrate.md` is
blocked because no second host can hold the VM, and shrinking it to 16 GB does
not fix that: pve1 has 28.2 GB total with 23 GB already allocated. That
runbook's capacity table dates from 2026-08-10 and is refreshed as part of this
work, but its blocked status stands.

**Cloud-init snippets remain host-local.** An earlier draft of this design
treated devbox's `cicustom: user=local:snippets/devbox-cloud-init.yaml` as a
migration blocker and proposed adding the `snippets` content type to
`truenas-vmdisks` to fix it. Checking that against Proxmox rather than
inferring it from config showed the premise was wrong:

```
root@pve5:~# pvesh get /nodes/pve5/qemu/111/migrate --target pve1 --output-format json
{
    "allowed_nodes": ["pve2", "pve4", "pve1", "pve3"],
    "local_disks": [],
    "local_resources": [],
    "running": 1
}
```

`local` is declared on every host (`dir: local`, `path /var/lib/vz`,
`shared 0`), so the storage ID resolves everywhere and the precondition check
passes with no local disks or resources reported. Migration is permitted to all
four other nodes.

The real exposure is narrower: the snippet *file* exists in exactly one place
per VM — `devbox-cloud-init.yaml` only on pve5, `haproxy-1-cloud-init.yaml`
only on pve1. A migration succeeds and the guest keeps running, but
regenerating the cloud-init drive on the new host would not find its user-data.
That is latent configuration drift rather than a blocker, it affects the
existing VMs equally, and it is left as follow-up work rather than being fixed
opportunistically here.

## 8. Follow-up work

**Physical RAM expansion.** `docs/ram-expansion-decision.md` had to bracket its
cost estimate across DDR4-vs-DDR5 and slot-availability unknowns, noting that
the repository documented no host models, DIMM types, or slot counts anywhere.
That gap is now closed by direct measurement:

```
# dmidecode -t memory
pve2/pve3/pve4:  Maximum Capacity: 32 GB / Number Of Devices: 2
                 Size: 16 GB   Type: DDR4  Speed: 2667 MT/s
                 Size: No Module Installed
pve1:            Maximum Capacity: 64 GB / 2x 16 GB DDR5-5600, both slots full
pve5:            Maximum Capacity: 128 GB / 4x 32 GB DDR5-4800, all slots full
```

The three tight hosts each have one empty DDR4-2667 SODIMM slot and a 32 GB
ceiling. That is the decision doc's cheapest bracket — a single 16 GB module
per host, no matched-pair swap required — confirmed rather than assumed. Adding
one module each roughly triples worker allocatable memory on the three tightest
nodes. Re-check pricing before acting; that document's market figures are
time-boxed to 2026-08-07 and the DRAM shortage it describes was still moving.

**iGPU UMA carve-out.** pve2/3/4 show 12.6 GB usable from 16 GB installed, and
pve1 28.2 GB from 32 GB — roughly 3.4 and 3.8 GB respectively consumed by the
integrated GPU framebuffer on headless hypervisors. Reducing the UMA allocation
in BIOS would return most of that, but requires physical console access to each
machine.

**`topology.kubernetes.io/zone` is unset on all 8 Kubernetes nodes.** Nothing
prevents a 3-replica Longhorn or CNPG spread from placing multiple replicas on
VMs that share one physical Proxmox host. `scenarios/pve5-worker-rebalance.md`
names this as a hard precondition for further worker consolidation and it was
never implemented.

**Three workers are effectively full.** `talos-2qd-v0u`, `talos-g1i-e3h` and
`talos-k3y-y3e` each sit at 98% memory requests against ~2.3 GiB allocatable.
The August analysis recorded them between 66% and 86%; they have tightened
since. They are the cluster's real scheduling bottleneck and the primary
beneficiary of the RAM expansion above.

**Cloud-init snippets on cluster-wide storage**, per §7.
