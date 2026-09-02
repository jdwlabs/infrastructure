# devbox2 — Lifeboat VM Provisioning and the pve5 Memory Reclaim

Status: **DESIGN — approved, not yet applied.** No `terraform apply` has been
performed or is proposed by this document. Capacity figures captured live
2026-09-01 via `pvesh` and `kubectl`; re-verify before acting if reading this
more than a few weeks later.

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
| Disk | 32 GB on `truenas-vmdisks` | NFS-backed, keeping the VM migratable and off pve1's local-lvm |
| Address | 192.168.1.57/24, gateway 192.168.1.254 | below the .64 DHCP pool floor, adjacent to devbox's .56 |
| Admin user | `dev-admin`, existing SSH key | existing Remote-SSH and tailnet habits carry over unchanged |
| Snippet datastore | `local` on pve1 | same pattern haproxy-1 already uses on this host; see §7 |
| `on_boot` | true | returns unattended after a pve1 reboot |
| Tags | `dev;lifeboat` | |

Toolchain installed via cloud-init: tailscale, git, chezmoi (which pulls the
dotfiles), kubectl, talosctl, the Claude Code CLI, and SSH keys. Deliberately
no Docker and no Node toolchain — build capability is what devbox is for, and
adding it here would grow the VM past the memory budget that makes it fit on
pve1 at all.

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

The ordering is not incidental — the reclaim stops the machine the operator
works from, so the lifeboat has to exist and be proven first.

1. `terraform apply` devbox2 on pve1. No downtime for anything existing.
2. Bootstrap and verify devbox2: tailnet reachable, `kubectl get nodes` returns
   8 Ready, `talosctl` reaches the control planes, `claude` starts.
3. **From devbox2:** apply the `dev_vm_memory` change and stop/start devbox.
4. Verify pve5 and devbox.

## 6. Verification

- `terraform fmt` and `terraform validate` clean; the plan reviewed by a human
  before any apply.
- devbox2 answers on both the tailnet and 192.168.1.57.
- From devbox2: `kubectl get nodes` returns 8 Ready; `talosctl` reaches the
  control planes; `claude` starts.
- After the resize: `free -g` on pve5 reports at least 11 GB free with all
  three guests running; devbox SSH is back; the cluster still shows 8 nodes
  Ready.
- The 02:00 `vzdump` of VM 111 succeeds on the following night. Seven healthy
  dailies exist today at ~19.6 GB each on `truenas-backup`.

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
