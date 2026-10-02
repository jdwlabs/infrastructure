# AI-SRE Local GPU Tier: Operator Runbook

Covers the `vllm-inference` VM (`terraform/gpu-node.tf`) — an RTX 5090 passthrough
host on `pve5` serving the AI-SRE agent's quota-immune local model tier. This is
day-two/rebuild operations, not the terraform apply itself (see the PR history
on `gpu-node.tf` for that).

**VM:** `vllm-inference`, static `192.168.1.50`, user `vllm`
**GPU:** RTX 5090, PCI `0000:01:00` (VGA + audio, IOMMU group 13), cluster mapping `gpu-rtx5090`
**Model:** `QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ`, served as `local-chat`, also answering to its previous name `qwen/qwen3-coder-30b-a3b` until all three consumers have moved (see [docs/vllm-serving.md](../docs/vllm-serving.md#renaming-the-served-model)) (AWQ 4-bit, ~15.7GiB — 30B-total/3.3B-active MoE coder; GQA keeps the 32k-ctx KV cache ~3GiB on the 32GiB card)

---

## 1. Network gotcha — the LAN gateway is `.254`, not `.1`

Nothing on this LAN answers ARP for `192.168.1.1`. The real default gateway,
confirmed from every Proxmox node's own routing table, is **`192.168.1.254`**.
`terraform/variables.tf` (`gpu_vm_gateway`) already defaults to `.254` — if a
future VM on this network loses all egress with ARP requests reaching the
bridge but never getting a reply, check this first before anything more exotic.

## 2. Host prerequisite — GPU bound to vfio-pci

Before the VM can start, the RTX 5090 must be off `nouveau` and bound to
`vfio-pci` on `pve5`. Run this block on `pve5` itself (SSH in, or the
Proxmox node's own console) — it is hypervisor config, not something the
`vllm-inference` VM or a workstation checkout of this repo ever touches:

```bash
cat > /etc/modprobe.d/vfio.conf <<'EOF'
options vfio-pci ids=10de:2b85,10de:22e8 disable_vga=1
softdep nouveau pre: vfio-pci
EOF
cat > /etc/modprobe.d/blacklist-nouveau.conf <<'EOF'
blacklist nouveau
options nouveau modeset=0
EOF
cat > /etc/modules-load.d/vfio.conf <<'EOF'
vfio
vfio_iommu_type1
vfio_pci
EOF
update-initramfs -u -k all
systemctl reboot
```

**pve5 is at `192.168.1.204`**, held by a gateway Fixed Allocation against a
plain `iface vmbr0 inet dhcp` host config. An earlier version of this paragraph
said it was `inet static` at `192.168.1.169` and would return there after a
reboot — that is wrong and now dangerously so: `.169` is dead, accepting no
connection on `22` or `8006`. The host-static config at `.169` sat inside the
DHCP pool and caused the 2026-08-06 duplicate-address outage; recovery moved
pve5 off it entirely.

It is still worth re-deriving live (`pvesh get /cluster/status` from another
node) before relying on any of these addresses for SSH: the reservations live in
the gateway's configuration, which a firmware update or factory reset can drop.
`docs/host-addressing.md` is the authority for all five host addresses and how
each is held. VM and mapping config keyed by node name is unaffected either
way.

Verify after reboot: `lspci -k -s 01:00.0 | grep "in use"` → `vfio-pci`.

The cluster PCI mapping also needs a `subsystem-id` or first VM start fails
with `PCI device mapping invalid ... missing expected property 'subsystem-id'`.
Run this on `pve5` too, after that reboot (the block above ends with
`systemctl reboot`, which ends whatever session ran it): `lspci` reads the
local PCI bus, so the `SUB=` line only produces a real value when it runs
on the host the card is physically in, and `pvesh` here is being used as
`pve5`'s own node CLI, not called remotely against the cluster API:

```bash
SUB=$(lspci -s 01:00.0 -vn | sed -n 's/.*Subsystem: \([0-9a-f:]*\).*/\1/p')
pvesh set /cluster/mapping/pci/gpu-rtx5090 \
  --map node=pve5,path=0000:01:00,id=10de:2b85,iommugroup=13,subsystem-id=$SUB
```

## 3. VM setup (fresh boot, e.g. after a terraform recreate)

Driver only. Ubuntu 24.04, cloud-init user from `gpu_vm_ssh_public_key`. Run
this on the `vllm-inference` VM itself, over SSH. This part is host-level —
`talops` does not install a GPU driver — so it stays a manual step even
though everything downstream of it is now automated.

vLLM itself runs containerized (`talops vllm apply`, see §4), so unlike the
old bare-metal venv this VM never JIT-compiles Triton/CUDA kernels and needs
no host-side CUDA toolkit or Python build chain — the container image
carries its own. `build-essential` and `linux-headers` remain, because the
driver package itself builds a kernel module against them:

```bash
# NVIDIA driver (Blackwell needs 570+ open kernel modules) + build deps for
# the driver's own kernel module build, plus curl for the NVIDIA Container
# Toolkit apt-repo step that follows (docs/vllm-serving.md#host-prerequisites).
sudo apt-get update
sudo apt-get install -y build-essential linux-headers-$(uname -r) curl
sudo apt-get install -y nvidia-driver-580-open || sudo apt-get install -y nvidia-driver-570-open
sudo reboot   # picks up the driver cleanly
```

The NVIDIA Container Toolkit's own apt repository is a second, separate
manual step, also run on the VM, that this runbook does not cover — see
[docs/vllm-serving.md#host-prerequisites](../docs/vllm-serving.md#host-prerequisites).

## 4. Serving

Everything past the driver is no longer a venv and a hand-written systemd
unit: serving is defined in
[`inference/vllm/serving.yaml`](../inference/vllm/serving.yaml) and converged
by `talops vllm apply --confirm` — run from a workstation checkout of this
repo, not on the VM. It installs a Podman Quadlet unit (`vllm-server.service`),
stages the pinned image and model revision while the previous server keeps
running, then stops the previous server, starts the new one, and only then
checks it answers serving.yaml's model — a failed check rolls back
automatically. See [docs/vllm-serving.md](../docs/vllm-serving.md) for the
model/version change procedure, the status columns, and rollback.

## 5. Verify

From a workstation checkout of this repo (SSHes to the VM itself):

```bash
talops vllm status
```

On the VM, `nvidia-smi --query-gpu=memory.used,utilization.gpu --format=csv,noheader`
should show ~30GiB used once a request is in flight.
