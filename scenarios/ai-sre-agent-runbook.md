# AI-SRE Local GPU Tier: Operator Runbook

Covers the `vllm-inference` VM (`terraform/gpu-node.tf`) — an RTX 5090 passthrough
host on `pve5` serving the AI-SRE agent's quota-immune local model tier. This is
day-two/rebuild operations, not the terraform apply itself (see the PR history
on `gpu-node.tf` for that).

**VM:** `vllm-inference`, static `192.168.1.50`, user `vllm`
**GPU:** RTX 5090, PCI `0000:01:00` (VGA + audio, IOMMU group 13), cluster mapping `gpu-rtx5090`
**Model:** `QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ`, served as `qwen/qwen3-coder-30b-a3b` (AWQ 4-bit, ~15.7GiB — 30B-total/3.3B-active MoE coder; GQA keeps the 32k-ctx KV cache ~3GiB on the 32GiB card)

---

## 1. Network gotcha — the LAN gateway is `.254`, not `.1`

Nothing on this LAN answers ARP for `192.168.1.1`. The real default gateway,
confirmed from every Proxmox node's own routing table, is **`192.168.1.254`**.
`terraform/variables.tf` (`gpu_vm_gateway`) already defaults to `.254` — if a
future VM on this network loses all egress with ARP requests reaching the
bridge but never getting a reply, check this first before anything more exotic.

## 2. Host prerequisite — GPU bound to vfio-pci

Before the VM can start, the RTX 5090 must be off `nouveau` and bound to
`vfio-pci` on `pve5`:

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

**pve5 comes back on a different DHCP IP after a reboot** (observed: `.204` →
`.169`). Re-derive it live (`pvesh get /cluster/status` from another node) —
don't assume the old IP still applies to SSH access, though existing VM/mapping
config keyed by node name is unaffected.

Verify after reboot: `lspci -k -s 01:00.0 | grep "in use"` → `vfio-pci`.

The cluster PCI mapping also needs a `subsystem-id` or first VM start fails
with `PCI device mapping invalid ... missing expected property 'subsystem-id'`:

```bash
SUB=$(lspci -s 01:00.0 -vn | sed -n 's/.*Subsystem: \([0-9a-f:]*\).*/\1/p')
pvesh set /cluster/mapping/pci/gpu-rtx5090 \
  --map node=pve5,path=0000:01:00,id=10de:2b85,iommugroup=13,subsystem-id=$SUB
```

## 3. VM setup (fresh boot, e.g. after a terraform recreate)

Driver + CUDA toolkit + vLLM. Ubuntu 24.04, cloud-init user from
`gpu_vm_ssh_public_key`.

```bash
# 1. NVIDIA driver (Blackwell needs 570+ open kernel modules) + build deps.
# python3-dev and ninja-build are NOT optional — vLLM JIT-compiles Triton/CUDA
# kernels on first request; without them it crash-loops with
# "No such file or directory: 'ninja'" or a gcc failure citing a missing
# Python.h, both well after the driver/model already loaded successfully.
sudo apt-get update
sudo apt-get install -y build-essential linux-headers-$(uname -r) curl \
  python3-dev ninja-build
sudo apt-get install -y nvidia-driver-580-open || sudo apt-get install -y nvidia-driver-570-open

# 2. CUDA toolkit — the Ubuntu-repo nvidia-cuda-toolkit package predates
# Blackwell (sm_120) support. Use NVIDIA's own repo, matched to the driver.
wget -q https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb
sudo apt-get update
sudo apt-get install -y cuda-toolkit-13-0

# 3. uv + vLLM in an isolated venv.
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
uv venv /home/vllm/vllm-env --python 3.12
uv pip install --python /home/vllm/vllm-env/bin/python vllm hf_transfer

# 4. systemd unit.
sudo tee /etc/systemd/system/vllm.service >/dev/null <<'EOF'
[Unit]
Description=vLLM OpenAI-compatible inference server (AI-SRE local tier)
After=network-online.target
Wants=network-online.target

[Service]
User=vllm
ExecStart=/home/vllm/vllm-env/bin/vllm serve QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ \
  --served-model-name qwen/qwen3-coder-30b-a3b \
  --host 0.0.0.0 --port 8000 \
  --quantization awq_marlin \
  --max-model-len 32768 \
  --enable-auto-tool-choice --tool-call-parser qwen3_xml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# systemd units do not read /etc/profile.d — nvcc and CUDA_HOME must be set
# here explicitly, or vLLM fails at model-load time with
# "Could not find nvcc and default cuda_home='/usr/local/cuda' doesn't exist"
# even though nvcc is installed and on the interactive-shell PATH.
sudo mkdir -p /etc/systemd/system/vllm.service.d
sudo tee /etc/systemd/system/vllm.service.d/cuda-path.conf >/dev/null <<'EOF'
[Service]
Environment=PATH=/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=CUDA_HOME=/usr/local/cuda
EOF

sudo systemctl daemon-reload
sudo systemctl enable vllm
sudo reboot   # picks up the driver cleanly
```

After reboot: `sudo systemctl start vllm`.

## 4. First-boot model download

The model download (~13GiB for the original `gpt-oss-20b`; ~16GiB for the
`Qwen3-Coder-30B-A3B-Instruct-AWQ` weights) over the default `huggingface_hub`
requests backend hung indefinitely for us — connections sat in `CLOSE-WAIT` against the
AWS CloudFront edge IPs backing the HF CDN with zero bytes moving, both on the
full download and on the lighter revision/etag re-check that runs on every
`vllm serve` start even once the model is cached. **This is separate from the
`.254` gateway issue** — it reproduced with correct routing in place.

Fix: force the Rust-based transfer client, which uses a different HTTP stack
and did not hang.

```bash
uv pip install --python /home/vllm/vllm-env/bin/python hf_transfer
# (older huggingface_hub: HF_HUB_ENABLE_HF_TRANSFER=1 env var. Recent versions
# use hf_transfer automatically once installed and warn the env var is
# deprecated — either way, having the package installed is what matters.)
```

If a download was interrupted mid-file under the old backend, clear the
partial snapshot before retrying — a stale `.incomplete` blob does not resume
cleanly and `HF_HUB_OFFLINE=1` will refuse to start with
`IncompleteSnapshotError` against it:

```bash
rm -rf ~/.cache/huggingface/hub/models--QuantTrio--Qwen3-Coder-30B-A3B-Instruct-AWQ
sudo systemctl restart vllm
```

## 5. Verify

```bash
curl -s http://192.168.1.50:8000/v1/models | grep qwen
curl -s http://192.168.1.50:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen/qwen3-coder-30b-a3b","messages":[{"role":"user","content":"say ok"}],"max_tokens":20}'
```

`nvidia-smi --query-gpu=memory.used,utilization.gpu --format=csv,noheader` should
show ~30GiB used once a request is in flight.
