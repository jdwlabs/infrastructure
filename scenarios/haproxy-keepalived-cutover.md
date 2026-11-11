# Runbook: Cut over to the keepalived HAProxy pair

Status: **NOT EXECUTED.** Re-derived 2026-11-11 against the live cluster
(read-only: SSH to VM 110 and pve1-5, `talops haproxy status|plan`, ARP probes,
`terraform plan` on a scratch copy). No step below has been run. Every
`terraform apply`, `qm` change and `talops haproxy apply` is run by a human at a
LAN terminal.

This is **Phase A** (JDWLABS-717): the pair, plain VRRP. External UDP and the
Tailscale route stay on `haproxy-1` and are Phase B (JDWLABS-722), a separate
window. Prerequisite: [jdwlabs/infrastructure#225](https://github.com/jdwlabs/infrastructure/pull/225)
(the image checksum bump) is merged first, or the pve5 image copy in step 3 fails
its checksum.

## Why

`haproxy-1` is the only load balancer. When pve1 died on 2026-10-01 the
Kubernetes API, the Talos API and all ingress were dark for 2h08m while the
cluster behind them was healthy. This runbook adds `haproxy-2` on a second host
and turns `192.168.1.199` into a virtual address keepalived moves to whichever
instance survives. DNS, kubeconfig, `talosconfig` and LAN clients keep pointing
at `.199`. Design and rationale: `docs/haproxy-vm-provisioning.md` §5.5.

## What you end up with

| | haproxy-1 | haproxy-2 |
|---|---|---|
| Proxmox host | **pve5** (stays) | **pve1** (new) |
| VMID | 110 | 113 |
| Own address | `A` = `192.168.1.60` (new) | `B` = `192.168.1.61` (new) |
| `vrrp_priority` | 150 | 100 |
| Holds `192.168.1.199` | **at the end**, and after any failover until the next hand move | after step 8 until step 13 |

`A` and `B` were probed on 2026-11-11 from pve1 and pve2 (no claimant; the
gateway and `.199` answered as positive controls), have no Service, Pod or
Endpoint in the cluster, and sit below the gateway's DHCP floor (`.64`). Neither
instance preempts: the address stays where it is when a failed instance
returns. That is a decision, not a default left in place.

The pair ends with `haproxy-1` holding the address because pve1 is the host that
powered off three times in five weeks; the address should live on the stable
host, together with the UDP and Tailscale pieces that cannot move yet.

What follows the address, and what does not:

| Service | After cutover | If pve5 dies | If pve1 dies |
|---|---|---|---|
| Kubernetes API, Talos API, HTTP(S) ingress | Follows `.199` | Moves to haproxy-2, seconds | Nothing |
| LAN DNS (`dnsmasq`, `.199:53`) | Follows it (`bind-dynamic`) | Moves | Nothing |
| Bedrock UDP from the LAN (`.199:19132`) | Follows it once haproxy-2 carries the rules (step 7) | Moves; sessions reset | Nothing |
| Bedrock UDP from the internet | **Stays on haproxy-1.** The gateway forward is attached to its MAC | Down until pve5 returns | Nothing |
| Tailscale subnet router | **Stays on haproxy-1** | Route down until pve5 returns | Nothing |

## Expected dark time, per step

Estimates, not measurements. The rehearsal (two containers) moved the address in
2.2 s on a holder kill and 3.9 s on a HAProxy stop. Step 13 measures the live
numbers; write them on the ticket.

| When | What is dark | Estimate | Why |
|---|---|---|---|
| Step 8, the `.199` swap | API, Talos API, ingress, LAN DNS; open connections reset; LAN Bedrock clients drop and rejoin through haproxy-2 | 6-10 s | haproxy-2's health check needs two passes (2 s apart) before it is eligible, then the master-down wait (about 3.6 s) |
| Step 10, apply that moves haproxy-1 to `A` | External Minecraft (FWB, crew lab, proving grounds, seed twin), remote NetherNet, the tailnet route to `192.168.1.0/24`. API, ingress and DNS are **not** affected: haproxy-2 holds `.199` | 1-3 min | The provider restarts VM 110; cloud-init re-runs and installs keepalived (`apt` over the internet); Tailscale reconnects. Longer if `apt` is slow |
| Step 13, `qm stop 113` | API, Talos API, ingress, LAN DNS | 3-5 s | master-down wait |
| Window as a whole | **No redundancy** between step 8 and the end of step 10 | 10-20 min | VIP lives on pve1 only; see "If pve1 dies mid-window" |

The previous version of this runbook shut `haproxy-1` down for steps 9-12, which
darkened UDP ingress and the tailnet route for the whole of that, human time
included. Moving `haproxy-1` off `.199` in place (step 8) removes that.

## The traps this sequence is built around

**1. A plan targeted at one instance still replaces the other's snippet.**
`-target` follows dependencies between *resources*, not instances. Targeting
`proxmox_virtual_environment_vm.haproxy["haproxy-2"]` pulls in every instance of
`proxmox_virtual_environment_file.haproxy_cloud_init` and of the image resource.
Step 3 therefore also replaces `haproxy-1`'s snippet (the destroy half deletes
the old copy on pve1, the create half writes the new one on pve5). Nothing
happens to VM 110 at that moment. At its next *cold start* it gets a new
cloud-init instance-id and re-runs cloud-init with the group's user-data.

That re-run is wanted, but only once its address has moved. Between step 3 and
step 10 the snippet says "group member at `A`" while VM 110's network still says
`.199`. keepalived started in that state deletes `.199` from the interface, so
the cloud-init installs an `ExecCondition` that skips keepalived unless the
instance's own address is configured. An unplanned `haproxy-1` reboot in that
window comes back as it is today. Keep the window short anyway.

**2. A new instance must not claim the address before it can serve.** A fresh VM
runs the distro's placeholder `haproxy.cfg`. The keepalived health check passes
only while HAProxy listens on `:6443`, so it stays out of the election until the
real config is pushed. Once pushed it becomes eligible, so step 5 stops
keepalived first and step 8 starts it at the moment `haproxy-1` lets go.

**3. Any cold start after a move is a cloud-init re-run.** VM 110 got a new
cloud-init instance on 2026-11-08 05:36 (the pve1 to pve5 move) with unchanged
user-data. The re-run on step 10 regenerates SSH host keys and, because the live
snippet is the pre-dnsmasq template, also rewrites `/etc/dnsmasq.conf` (identical
to live), adds `99-ip-forward.conf` and installs keepalived. It does not touch
`/etc/minecraft-udp-nat.nft`, its loader unit or `/var/lib/tailscale`.

## Preconditions: hard gates, all must pass

Set these in every terminal you use, and do the work from a **LAN host** with an
address of its own in `192.168.1.0/24`, not a tailnet client reaching the LAN
through haproxy-1's subnet route (step 10 takes that route down).

```bash
export A=192.168.1.60 B=192.168.1.61 VIP=192.168.1.199
export KEY=~/.ssh/<key>           # a key root@pve1, root@pve5 and haproxy-admin@ accept
alias talops="$HOME/.cache/talops/talops -k $KEY"
```

1. **#225 merged, `talops` built from the merged `main`:**
   ```bash
   git switch main && git pull --ff-only
   (cd bootstrap && go build -buildvcs=false -o ~/.cache/talops/talops . ; echo "exit=$?")
   terraform version | head -1                # needs >= 1.17.0 (devbox: 1.17.1)
   grep -A3 'variable "haproxy_cloud_image_checksum"' terraform/variables.tf | grep default
   curl -s https://cloud-images.ubuntu.com/releases/noble/release/SHA256SUMS | grep 'ubuntu-24.04-server-cloudimg-amd64.img$'
   ```
   The two hashes must be equal (`04e8f246...` on 2026-11-11). The image URL is
   rolling: if Ubuntu respun it since, stop and bump the default by PR first.
   Trust the exit code of the build; under RTK a failure can print a success line
   (`~/.local/share/chezmoi/docs/agent-tooling-traps.md`).
2. **The standalone report is clean** (checked 2026-11-11: `ssh: ok`,
   `service: active`, `configDrift: false`, `backendsUp: 16/16`, `plan: 0 changes`):
   ```bash
   talops haproxy status && talops haproxy plan
   ```
   `VM state unread: terraform show` in the notes only means this checkout has no
   Terraform backend yet; it is not a failure.
3. **`A` and `B` are free**, re-proven on the night. Check from a Proxmox host on
   the LAN segment. `arping` is not installed on pve1-pve5 and installing a
   package on a hypervisor is a live change, so use the probe in
   `scenarios/files/arp-dup-probe.py` (same frames as `arping -D`). Run the
   controls separately: they **must** be claimed, `A` and `B` **must not**. Run
   from the repo root.
   ```bash
   P=scenarios/files/arp-dup-probe.py
   ssh root@pve1 "python3 - vmbr0 3 192.168.1.254 $VIP" < $P ; echo "controls exit=$?"   # 1, and both lines show a claimant MAC
   ssh root@pve1 "python3 - vmbr0 3 $A $B"              < $P ; echo "A,B exit=$?"       # 0: no claimant
   ping -c2 -W1 $A; ping -c2 -W1 $B                       # 100% loss
   ```
   Then read the gateway (`192.168.1.254`, Home Network → IP Allocation / Device
   List) for `.60` and `.61`, including *Fixed Allocation* rows for devices that
   are switched off: a silent address is not a free one. Record both addresses
   on the ticket. If either is taken, pick the next free address below `.64` and
   repeat; use it everywhere below.
4. **VMID 113 is free:**
   ```bash
   ssh root@pve1 "pvesh get /cluster/resources --type vm --output-format json" | jq -r '.[].vmid' | sort -n | tr '\n' ' '
   ```
5. **pve1 has room and is cool.** Both hosts healthy and quorate, pve1 at least
   2 GiB available, and pve1's CPU below **85 °C** (both series, every sample, for
   a minute). On 2026-11-11 pve1 read 82-86 °C at load 1.4, which is **not** a go.
   ```bash
   ssh root@pve1 'free -m | sed -n 2p; uptime'
   ssh root@pve2 'pvecm status | grep -E "Quorate|Nodes:"'
   for i in $(seq 12); do curl -s -m3 http://192.168.1.200:9100/metrics | awk '
     /^node_hwmon_temp_celsius\{chip="pci0000:00_0000:00:18_3",sensor="temp1"\}/{t=$2}
     /^node_thermal_zone_temp\{/{z=$2} END{printf "tctl=%s acpitz=%s\n",t,z}'; sleep 5; done
   ```
6. **`local` on pve1 offers `snippets` and `import`**, and pve5's did at the plan
   (it holds the existing image and snippet):
   ```bash
   ssh root@pve2 "pvesh get /nodes/pve1/storage --output-format json" | jq -r '.[] | select(.storage=="local") | .content'
   ```
7. **No other VRRP group uses router id 51.** Watch ten seconds on `haproxy-1`;
   expect no packets. If something shows up, set `haproxy_vrrp_router_id` in
   step 1.
   ```bash
   ssh haproxy-admin@$VIP 'sudo timeout 10 tcpdump -ni eth0 -c5 vrrp'
   ```
8. **Nothing allowlists `.199` as a *source*, and nothing SSHes to `.199` for
   `haproxy-1`.** After cutover the instances originate traffic from `A` and `B`,
   and `.199` is whichever instance holds it. Blackbox probes of
   `192.168.1.199:6443` keep working; expect them to blink once at step 8.
9. **Secrets are hydrated, including the remote-state credentials**
   (`docs/secrets.md`), and `terraform init` has been run in `terraform/`.
10. **Backup of VM 110 is fresh.** The nightly job (01:30, `truenas-backup`) must
    have an entry newer than any change to the VM; if in doubt, take one now. No
    restore has ever been drilled, so this is a last resort, not a plan.
    ```bash
    ssh root@pve5 'pvesm list truenas-backup --vmid 110 | tail -3'
    ssh root@pve5 'vzdump 110 --storage truenas-backup --mode snapshot'     # optional; no outage
    ```
11. **A quiet window.** No human players on FWB, the crew lab, proving grounds or
    the seed twin (the cutover resets every LAN connection and darkens external
    play for 1-3 minutes). The FWB nightly restart (16:40 UTC) must not overlap.
    Josh and the players are told in advance.
12. **ssh-agent holds a key** `root@pve1` and `root@pve5` accept. The provider
    uploads snippets and imports disks over node SSH and reads keys only from an
    agent (`terraform/providers.tf`):
    ```bash
    ssh-add -l || { eval "$(ssh-agent -s)" && ssh-add $KEY; }
    ssh root@pve1 true && ssh root@pve5 true
    ```
13. **`A` and `B` have no `known_hosts` entries**, stale or otherwise:
    ```bash
    for ip in $A $B; do ssh-keygen -F "$ip"; done        # expect: no output
    ```
14. **`haproxy-1` can promote a secondary address.** Step 8 deletes its primary
    `.199` while `A` rides along as a secondary; without promotion the kernel
    drops `A` too and the VM is left with no address.
    ```bash
    ssh haproxy-admin@$VIP 'sysctl net.ipv4.conf.eth0.promote_secondaries'   # expect = 1
    ```
15. **Nothing will reconfigure `haproxy-1`'s network during the window.** Its
    netplan still says `.199` until step 10, so a networkd restart puts `.199`
    back on it while `haproxy-2` holds it. Check the timers and stay clear:
    ```bash
    ssh haproxy-admin@$VIP 'systemctl list-timers apt-daily-upgrade.timer apt-daily.timer --no-pager'
    ```

## Stage: nothing here touches production traffic

1. **Edit the vaulted tfvars** in your own terminal. The VRRP secret is created in
   this edit and goes nowhere else: not a shell history, not an agent session.
   ```bash
   talops secrets edit tfvars
   ```
   ```hcl
   haproxy_ip = "192.168.1.199"        # unchanged: now the virtual address

   haproxy_vms = [
     { node_name = "pve5", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.60/24", vrrp_priority = 150 },
     { node_name = "pve1", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.61/24", vrrp_priority = 100 },
   ]

   haproxy_vrrp_auth_pass = "<8 letters or digits>"
   ```
   Keep `haproxy-1`'s `cpu_cores`, `memory` and `disk_size` exactly as they are;
   only `ip` changes and `vrrp_priority` is added. No checksum override: #225
   carries it.

   From this edit until step 10, `talops haproxy` without `--host` targets `A`
   and `B`, and `A` does not exist yet. Use `--host` as shown and do not run
   `talops reconcile`.

   *Abort here:* `git restore terraform/terraform.tfvars.enc.yaml && rm terraform/terraform.tfvars && talops secrets hydrate`.
2. **Plan the new instance only:**
   ```bash
   cd terraform
   terraform plan -target='proxmox_virtual_environment_vm.haproxy["haproxy-2"]' -out=tfplan-lb2
   terraform show tfplan-lb2 | less
   ```
   **Expect `4 to add, 1 to change, 1 to destroy`, and exactly:**
   - create `download_file.haproxy_cloud_image["pve5"]` (the image haproxy-1 imports from)
   - create `file.haproxy_cloud_init["haproxy-2"]`, create `vm.haproxy["haproxy-2"]` (new VM on pve1)
   - **replace** `file.haproxy_cloud_init["haproxy-1"]` (`node_name` pve1 to pve5 and a sensitive content change; trap 1)
   - **update in place** `download_file.haproxy_cloud_image["pve1"]` (`overwrite` true to false only)

   **Abort** on anything else: any `vm.haproxy["haproxy-1"]` line, any `talos-*`,
   `dev_vm`, `devbox2` or `vllm-inference` line, a replacement of either image,
   anything destroyed other than the one snippet.
3. **Human applies:** `terraform apply tfplan-lb2`. **The trap-1 window opens
   here.** The apply downloads the 600 MB image to pve5 (a minute or two). If it
   stops at the snippet with "already exists" (a hand-staged copy sits on pve5),
   VM 110 is unaffected and still boots from that copy, but the old pve1 copy is
   already deleted: read the error and re-plan. If the new plan is a lone create
   of `haproxy-1`'s snippet on pve5 and the provider refuses to overwrite the
   hand-staged file, move that file aside on pve5
   (`mv /var/lib/vz/snippets/haproxy-1-cloud-init.yaml{,.staged}`; VM 110 reads it
   only at its next cold start) and apply; keep `.staged` until step 10 is done.
   *Abort:* "Abort before step 8" below.
4. **Trust `haproxy-2`'s host keys.** Read the fingerprints through the guest
   agent and check the scanned keys against them before appending. The agent is
   installed by cloud-init, so the first command fails until that is far enough;
   retry it.
   ```bash
   ssh root@pve1 "qm guest exec 113 -- sh -c 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf \$f; done'"
   ssh-keyscan -t rsa,ecdsa,ed25519 $B > "$XDG_RUNTIME_DIR/haproxy-2.keys"
   ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-2.keys"       # every line must match one above
   cat "$XDG_RUNTIME_DIR/haproxy-2.keys" >> ~/.ssh/known_hosts
   ssh haproxy-admin@$B '
     cloud-init status --wait
     systemctl show keepalived -p ExecCondition | grep -c "inet '"$B"'/"       # expect 1
     test -x /usr/local/sbin/haproxy-vip-ready && echo check-present
     ip -4 -o addr show eth0                                                    # expect .61 only, no .199
     sysctl net.ipv4.ip_nonlocal_bind net.ipv4.ip_forward                       # expect both = 1
   '
   ```
   `.199` on `haproxy-2` here means trap 2 failed: `sudo systemctl disable --now keepalived`
   on it immediately and do not continue.
5. **Take `haproxy-2` out of the election, then give it the real config.** The
   push is what makes it eligible, so the order matters:
   ```bash
   ssh haproxy-admin@$B 'sudo systemctl disable --now keepalived'
   talops haproxy plan   --host $B       # binds 192.168.1.199, not .61
   talops haproxy apply  --host $B
   talops haproxy status --host $B       # backendsUp must read 16/16
   ```
   The config binds `.199` on a host that does not hold it; that works because
   cloud-init set `net.ipv4.ip_nonlocal_bind`. `cannot bind socket` means the
   sysctl is missing: stop.
6. **Prove the two instances can exchange VRRP on the addresses they will use.**
   Adverts are unicast IP protocol 112; if anything between pve5 and pve1 drops
   them, both become master and step 8 ends with `.199` on two hosts. `haproxy-1`
   does not have `A` yet, so lend it `A` as a second address. **Leave it there**:
   step 8 needs it, and it is not persisted (a reboot removes it).
   ```bash
   ssh haproxy-admin@$VIP "sudo ip addr add $A/24 dev eth0"
   ping -c2 -W1 $A                                                          # now answers

   # Trust A now so step 8's ssh to it cannot stop at a host-key prompt. It is the
   # same machine as .199, so its keys must equal the ones .199 already presents.
   ssh haproxy-admin@$VIP 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf $f; done'
   ssh-keyscan -t rsa,ecdsa,ed25519 $A > "$XDG_RUNTIME_DIR/haproxy-1-pre.keys"
   ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-1-pre.keys"       # every line must match one above
   cat "$XDG_RUNTIME_DIR/haproxy-1-pre.keys" >> ~/.ssh/known_hosts
   ssh -o BatchMode=yes haproxy-admin@$A true && echo A-ssh-ok

   # A -> B
   ssh -n haproxy-admin@$B "sudo timeout 8 tcpdump -ni eth0 -c1 'ip proto 112 and src $A and dst $B'" &
   sleep 3
   ssh haproxy-admin@$VIP "sudo python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_RAW, 112); s.bind((\"$A\", 0)); s.sendto(bytes(8), (\"$B\", 0))'"
   wait

   # B -> A
   ssh -n haproxy-admin@$VIP "sudo timeout 8 tcpdump -ni eth0 -c1 'ip proto 112 and src $B and dst $A'" &
   sleep 3
   ssh haproxy-admin@$B "sudo python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_RAW, 112).sendto(bytes(8), (\"$A\", 0))'"
   wait
   ```
   Each `tcpdump` must print one packet (`-n` and the longer `sleep` keep the
   background `ssh` from stopping on the terminal and give `sudo` time to start). `0 packets captured` in either direction
   is a hard stop: find what filters protocol 112 between pve1 and pve5 first.
   *Abort:* `ssh haproxy-admin@$VIP "sudo ip addr del $A/24 dev eth0"`, then "Abort before step 8".
7. **Copy the live UDP rules to `haproxy-2`** so LAN Bedrock clients follow the
   address. Copy the live file; do not regenerate it: `internal/udpnat` cannot
   render it (it emits UDP only, one target, ports to 19141; live has 86 rules
   with TCP signalling on 19132-19135, ports to 19219 and two target nodes).
   Only the table name changes, to the convention in
   `scenarios/bedrock-external-access.md` ("Placing the nftables rules"), so the
   new host does not become a second one to migrate off `minecraft`.
   ```bash
   scp haproxy-admin@$VIP:/etc/minecraft-udp-nat.nft "$XDG_RUNTIME_DIR/rules.nft"
   sha256sum "$XDG_RUNTIME_DIR/rules.nft"      # 78ad3f0c... on 2026-11-11 (188 lines); if it differs, the rules changed: read the diff
   grep -c 'table ip minecraft' "$XDG_RUNTIME_DIR/rules.nft"      # expect 3
   sed 's/table ip minecraft/table ip udpnat/' "$XDG_RUNTIME_DIR/rules.nft" > "$XDG_RUNTIME_DIR/udpnat.nft"
   scp "$XDG_RUNTIME_DIR/udpnat.nft" haproxy-admin@$B:/tmp/udpnat.nft
   ssh haproxy-admin@$B '
     sudo install -d /etc/nftables.d && sudo install -m 0644 /tmp/udpnat.nft /etc/nftables.d/udpnat.nft && rm /tmp/udpnat.nft
     printf "%s\n" "[Unit]" "Description=UDP ingress DNAT rules" "After=network-online.target" "Wants=network-online.target" "" \
       "[Service]" "Type=oneshot" "RemainAfterExit=yes" "ExecStart=/usr/sbin/nft -f /etc/nftables.d/udpnat.nft" "" \
       "[Install]" "WantedBy=multi-user.target" | sudo tee /etc/systemd/system/udpnat-rules.service >/dev/null
     sudo systemctl daemon-reload && sudo systemctl enable --now udpnat-rules.service
     sudo nft list table ip udpnat | grep -c dnat           # expect 86
     sudo nft list table ip udpnat | grep -c masquerade     # expect 86
     systemctl is-enabled udpnat-rules nftables              # enabled, disabled
     sysctl net.ipv4.ip_forward                              # = 1
   '
   ```
   Never `nft flush ruleset` on these VMs and never enable `nftables.service`.
   The rules match on port, not address, so they are inert on an instance that is
   not receiving the traffic. Until Phase B generates them, a rule change is made
   on both instances, and never load both tables on one.

## Cut over: the endpoint is dark for about 6-10 s

Open a second terminal on the LAN host and leave this running through step 13; it
is the measurement:

```bash
while :; do printf '%s ' "$(date +%T.%N | cut -c1-12)"; curl -sk -m1 -o /dev/null -w '%{http_code}\n' https://192.168.1.199:6443/version; sleep 0.5; done
```

8. **Swap the address: one command line, nothing typed in between.** `A` is
   already on `haproxy-1` (step 6), so this session rides `A`, and `A` is promoted
   to primary when `.199` goes. `haproxy-2` starts keepalived as the next thing
   that happens.
   ```bash
   ssh haproxy-admin@$A 'sudo ip addr del 192.168.1.199/24 dev eth0' && ssh haproxy-admin@$B 'sudo systemctl enable --now keepalived'
   ```
   `&&`, not `;`: if the first `ssh` fails (host key, network, `sudo`), `.199` is
   still on `haproxy-1` and keepalived must not start, or `haproxy-2` becomes
   master with `.199` on both. Step 6 trusted `A` so this cannot stop at a prompt.
   If it fails, check `ip -4 -o addr show eth0` on both before doing anything else.
   `haproxy-1` stays up with the same MAC, so Tailscale and the gateway's
   MAC-keyed forward keep working. If the gateway instead forwards to the stale IP
   `.199`, packets land on `haproxy-2` and its rules DNAT them: working either way.
9. **Prove it from the LAN**, not from the instance:
   ```bash
   kubectl get --raw /version
   talosctl -e $VIP -n <cp-ip> version
   curl -skI https://<an-ingress-host>/ | head -1
   dig +short @$VIP cluster.jdwlabs.com                    # expect 192.168.1.199
   ssh haproxy-admin@$B 'ip -4 -o addr show eth0'          # expect .61 and .199 secondary
   ssh haproxy-admin@$A 'ip -4 -o addr show eth0; ip -4 route show default'   # expect .60 primary only, default via .254
   ```
   If the new `.199` holder does not appear within about 15 s (the `ip addr` above
   shows `.61` alone), look at `journalctl -u keepalived -n 30` on `haproxy-2`
   before concluding anything; the address does not arrive at once.
   **Rollback from here until step 10 starts:**
   ```bash
   ssh haproxy-admin@$B 'sudo systemctl disable --now keepalived'
   ssh haproxy-admin@$A 'sudo ip addr add 192.168.1.199/24 dev eth0'
   ssh haproxy-admin@$A 'sudo python3 - eth0 192.168.1.199' < scenarios/files/arp-announce.py
   ```
   `ip addr add` announces nothing: until the announcement, every neighbour and
   the gateway still map `.199` to haproxy-2's MAC. If haproxy-2 is up it
   forwards to haproxy-1 meanwhile; if its host is gone they black-hole `.199`
   for tens of seconds (cache lifetime) without it. Dark: about 5 s plus the
   announcement; the frame layout is unit-checked but has not been sent on this
   LAN, so confirm with `ip neigh show $VIP` on the loop host. `A` is not
   persisted and netplan still says `.199`, so a `haproxy-1` reboot also restores
   the original state.

   **If pve1 dies mid-window** (between step 8 and the end of step 10 the address
   lives on `haproxy-2` alone, and `haproxy-1` has no keepalived yet): run the
   last two lines above at once (the first will fail, its host is gone). Only
   before `terraform apply tfplan-lb1` starts: after that `haproxy-1` is
   rebooting, and when it is back its own keepalived takes the address.
10. **Move `haproxy-1` to its own address.** Human plan and apply:
    ```bash
    cd terraform
    terraform plan -target='proxmox_virtual_environment_vm.haproxy["haproxy-1"]' -out=tfplan-lb1
    terraform show tfplan-lb1 | less
    ```
    Expect **0 to add, 1 to change, 0 to destroy**: only
    `initialization.ip_config.ipv4.address` `192.168.1.199/24` to `192.168.1.60/24`.
    `2 to change` is acceptable if the second is the `overwrite` update on the
    pve1 image. **Abort on any `disk` / `import_from` line, any replacement, or
    any other resource**: add `import_from` to `ignore_changes` by PR first. Then:
    ```bash
    terraform apply tfplan-lb1
    ```
    The provider restarts VM 110 (`reboot_after_update`, read in the pinned
    provider source and not yet observed here), cloud-init re-runs, and the VM
    comes back with new SSH host keys. Confirm rather than assume; start it by
    hand if it is still down:
    ```bash
    ssh root@pve5 'qm status 110; qm config 110 | grep ipconfig0'     # expect ip=192.168.1.60/24
    ssh root@pve5 'qm start 110'                                      # only if stopped
    ```
    **The trap-1 window closes here.** API, ingress and DNS are unaffected because
    `haproxy-2` holds `.199`; external Minecraft and the tailnet route are down
    until this finishes. If `haproxy-1` does not come back, the endpoint is fine:
    fix it at leisure.
11. **`haproxy-1` returns as a backup.** Trust its new host keys the way step 4
    did, and drop the old one:
    ```bash
    ssh-keygen -R $VIP                                     # the address answers as whichever instance holds it
    ssh-keygen -R $A                                       # step 6 trusted the pre-reboot keys
    ssh root@pve5 "qm guest exec 110 -- sh -c 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf \$f; done'"
    ssh-keyscan -t rsa,ecdsa,ed25519 $A > "$XDG_RUNTIME_DIR/haproxy-1.keys"
    ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-1.keys"       # every line must match one above
    cat "$XDG_RUNTIME_DIR/haproxy-1.keys" >> ~/.ssh/known_hosts
    ssh haproxy-admin@$A '
      cloud-init status --wait
      ip -4 -o addr show eth0                              # expect .60 only
      systemctl is-active haproxy keepalived dnsmasq minecraft-udp-nat tailscaled
      sudo tailscale status | head -3
      sudo nft list table ip minecraft | grep -c dnat      # expect 86; counters keep rising
    '
    ```
    `.199` in that `ip` output has two causes with opposite responses; look at
    `haproxy-2` before touching anything:
    ```bash
    ssh haproxy-admin@$B 'ip -4 -o addr show eth0; systemctl is-active keepalived haproxy; ss -Hltn "sport = :6443" | wc -l'
    ```
    - **`haproxy-2` holds `.199` too, both units active, listener present:** the
      instances cannot hear each other and both are master. Stop keepalived on
      `haproxy-1` (`sudo systemctl stop keepalived`), then compare the two
      `keepalived.conf` files and redo the protocol 112 check from step 6.
    - **`haproxy-2` does not hold `.199`:** not a split. It dropped out after
      step 8 and `haproxy-1` took over as designed. **Leave its keepalived
      running** and find out why `haproxy-2` left (`journalctl -u keepalived -n 50`).
    - **`haproxy-2` holds `.199` but is not serving:** stop keepalived on
      `haproxy-2`, not on `haproxy-1`.

## Verify

12. The pair, in one call:
    ```bash
    talops haproxy status
    ```
    Expect `holder: haproxy-2`, both rows `ssh: ok`, `service: active`,
    `keepalived: active`, `configDrift: false`, `backendsUp: 16/16`, no `error:`
    line. Then `talops haproxy apply`: expect `0 changes pushed`.
13. **Failover test: the exit criterion, and it puts the address on pve5.** The
    loop from the cut-over section is still running. Take the holder's host out
    from under it the way an outage would:
    ```bash
    ssh root@pve1 'qm stop 113'
    ```
    Expect the loop to miss for under five seconds, then `talops haproxy status`
    to report `holder: haproxy-1` and fail with `instance_unreachable` for
    `haproxy-2`, with ingress and DNS answering (`dig +short @$VIP cluster.jdwlabs.com`).
    Start it again (`ssh root@pve1 'qm start 113'`) and confirm the holder is
    **still** `haproxy-1` (no preemption) and the report is clean. If the address
    is not on `haproxy-1` after 15 s, stop and read `journalctl -u keepalived -n 50`
    on both instances before touching anything.
    The reverse test (stopping `haproxy-1`) is Phase B: it darkens UDP and the
    tailnet route. The HAProxy-stop health path is covered by the rehearsal (3.9 s).
14. **External paths, from off the LAN** (phone on cellular, an off-LAN tailnet
    peer). Record whether the gateway kept the forward on `haproxy-1`'s MAC,
    which now has `A` as its address; that answers the open question.
    - FWB joins from cellular (`scenarios/bedrock-external-access.md`, Step 4).
    - From the tailnet peer: reach a LAN-only address.
    If either fails, re-attach the service to the same device in the gateway UI
    (a human step; the gateway is not touched by this runbook otherwise).
15. **Record on the ticket:** `A`, `B`, the dark intervals read off the loop,
    `talops haproxy status` output from steps 12 and 13, the gateway result of
    step 14, and the sha256 of the rules file.

## Close out

16. **Commit the re-sealed tfvars.** Step 1 re-sealed
    `terraform/terraform.tfvars.enc.yaml` and so far that exists only in this
    checkout. Every other checkout still hydrates the standalone definition from
    `main`, and a plan from one proposes destroying `haproxy-2`. Land it through a
    pull request (R3: the owner merges), never a direct push to `main`:
    ```bash
    git status --short terraform/        # expect terraform.tfvars.enc.yaml, modified
    git switch -c chore/JDWLABS-717-declare-haproxy-pair
    git add terraform/terraform.tfvars.enc.yaml
    git commit -m "chore(secrets): declare the HAProxy keepalived pair"
    git push -u origin HEAD
    ```
    Not earlier: merged before the cutover, `main` would describe a pair that does
    not exist and `talops haproxy` from any checkout would target a `B` nothing
    answers on.
17. **Update the runbooks this cutover makes wrong**, in one pull request:
    - [ ] `scenarios/haproxy-vm-rebuild.md`, "Emergency rebuild: the original host
      is offline": losing pve1 no longer darkens the endpoint, so the premise and
      urgency go. VMID 113 is taken; a replacement is a group member at its own
      address, not a VM on `.199`, so its tfvars entry, host-key and SSH commands
      against `.199`, and the `onboot` disarm all change. VM 113 needs the backup
      decision VM 110 got.
    - [ ] `scenarios/proxmox-host-dark.md`: pve1 down is no longer "no API
      endpoint". The post-recovery checks SSH to `.199` and look for the `minecraft`
      table; they become per-instance, and the returned instance is a backup.
    - [ ] `scenarios/bedrock-external-access.md`: the device tables and Step 3 give
      the VM as `.199`; `haproxy-1` is now `A` with the same MAC, plus step 14's
      result. Its port convention still says 19132-19141 and `udpnat`; live is ports
      to 19219, TCP signalling on 19132-19135 and table `minecraft` on haproxy-1.
    - [ ] JDWLABS-722's definition of done says the "instance on pve1 stopped"; it
      is the pve5 instance (`haproxy-1`).
    The old snippet on pve1 (`/var/lib/vz/snippets/haproxy-1-cloud-init.yaml`) is
    destroyed by step 3's replacement; delete it by hand only if Terraform did
    not. The pve1 image now belongs to `haproxy-2`: keep it.

## Operating the pair afterwards

- **Never SSH or push to `192.168.1.199`.** It reaches whichever instance holds
  it, under that instance's host key. `talops haproxy` addresses each instance at
  its own address; `--host <A|B>` narrows to one.
- **One instance down is a failing status, by decision.** `talops haproxy status`
  exits 1 with `instance_unreachable`: the endpoint is up and the pair has no
  redundancy. `reconcile` still pushes to the reachable instance, then exits
  non-zero naming the one it skipped. Run `talops haproxy apply` once it is back.
- **To move the address by hand**, stop keepalived on the holder
  (`sudo systemctl stop keepalived`), wait for the other to take it, start it
  again. It will not take the address back.
- **Any cold start after a move re-runs cloud-init and regenerates SSH host
  keys**, whether or not the snippet changed (trap 3). Reboot the backup, never
  the holder, and re-trust its keys as in step 11.
- **Changing the cloud-init template** replaces both snippets in one apply (trap
  1) and arms a re-run on each instance's next cold start. Reboot them one at a
  time, backup first, moving the address in between.

## Abort before step 8

Valid from step 3 (the apply) to step 7. `haproxy-1` is untouched apart from the
snippet replacement of trap 1; it still holds `.199` and nothing depends on
`haproxy-2`.

1. `ssh haproxy-admin@$B 'sudo systemctl disable --now keepalived'`, then
   `ssh root@pve1 'qm shutdown 113'`.
2. If step 6 ran: `ssh haproxy-admin@$VIP "sudo ip addr del $A/24 dev eth0"`.
3. Vaulted tfvars: remove the `haproxy-2` entry, set `haproxy-1`'s `ip` back to
   `192.168.1.199/24` and drop `vrrp_priority`; keep `haproxy_vrrp_auth_pass`.
4. Plan scoped to the three load-balancer resources and read it:
   ```bash
   terraform plan -out=tfplan-abort \
     -target=proxmox_virtual_environment_vm.haproxy \
     -target=proxmox_virtual_environment_file.haproxy_cloud_init \
     -target=proxmox_virtual_environment_download_file.haproxy_cloud_image
   ```
   Expect `haproxy-2`'s VM and snippet and the pve1 image destroyed, and
   `haproxy-1`'s snippet replaced back to the standalone content. **No change to
   VM 110.** (The pve5 image stays: `haproxy-1` imported from it.) Anything
   touching VM 110, stop. Human applies it.
5. `talops haproxy status` is the standalone report at `.199`. The next cold
   start of `haproxy-1` still re-runs cloud-init (trap 3).

## Roll back to a standalone load balancer

Only `haproxy-1` can be the standalone one without also moving Tailscale and the
gateway forward. Before step 3 the abort line in step 1 is enough; from step 3 to
step 7 use "Abort before step 8"; between step 8 and step 10 use the step 9
rollback. After step 10 use this:

1. Make `haproxy-1` the holder (move the address by hand, above). It already is
   after step 13.
2. `ssh root@pve1 'qm shutdown 113'`.
3. Vaulted tfvars: remove the `haproxy-2` entry, set `haproxy-1`'s `ip` back to
   `192.168.1.199/24`, drop `vrrp_priority`. Leave `haproxy_vrrp_auth_pass`.
4. Plan scoped to the three load-balancer resources, so nothing pending rides
   along, and read it. Planning changes nothing, so do it while the endpoint is up:
   ```bash
   terraform plan -out=tfplan-standalone \
     -target=proxmox_virtual_environment_vm.haproxy \
     -target=proxmox_virtual_environment_file.haproxy_cloud_init \
     -target=proxmox_virtual_environment_download_file.haproxy_cloud_image
   ```
   Expect `haproxy-2`'s VM, snippet and pve1 image destroyed, `haproxy-1`'s snippet
   replaced, and its address changed in place.
5. On `haproxy-1`, only once the plan is accepted, and before the apply changes its
   address underneath keepalived: `sudo systemctl disable --now keepalived`. This
   drops `.199`; the endpoint is dark until step 6 finishes (about 3-5 minutes).
6. Human applies `tfplan-standalone`; confirm `haproxy-1` restarts at `.199`
   (re-trust its host keys, step 11 pattern).
7. `talops haproxy status`: the standalone report, `host: 192.168.1.199`.
8. Commit the re-sealed tfvars through a pull request, as in step 16, and revert
   whatever step 17 changed. Restoring from the vzdump is the last resort.
