# Runbook: Rebuild the HAProxy load-balancer VM

Status: **EXECUTED, 2026-08-09** (commit `568b368`). `haproxy-1` (VMID 110 on
pve1) is the live production load balancer at `192.168.1.199`. `haproxy-0`
(VMID 100) is gone: no VMID 100 exists on any node as of 2026-10-02. Every
`terraform apply` in this runbook is executed by a human. The agent contract
forbids autonomous applies, and this VM carries the Kubernetes API, the Talos
API, and all HTTP(S) ingress. Kept here as the tested recovery path for the
next rebuild, not as a pending TODO.

**The tested path assumes the VM's Proxmox host is up.** With pve1 offline it
fails at the first `terraform plan` — found on 2026-10-01, when pve1 died and
took the load balancer with it. The variant for that case is
[Emergency rebuild: the original host is offline](#emergency-rebuild-the-original-host-is-offline),
and it has **not** been executed. What to try before rebuilding anything is in
`scenarios/proxmox-host-dark.md`.

## Why

Until 2026-08-09, the load balancer fronting the cluster (`haproxy-0`, VMID
100 on pve1) was built by hand in the Proxmox UI. Its `haproxy.cfg` was
already fully automated — the generator renders backends from live cluster
state and pushes them with validation and rollback — but the *VM* was not
reproducible: if the disk or its host was lost, recovery was an ad-hoc manual
rebuild while the API and all ingress stayed dark.

`terraform/haproxy-node.tf` is what closed that gap. This runbook was its
driver: it provisioned a replacement, proved it served traffic, and cut over.
It stays here, already executed once, as the tested path for the next time a
rebuild is needed.

## Why this is a rebuild and not an import

`haproxy-0` cannot be adopted into Terraform as-is. Its live config carries no
cloud-init drive and no EFI disk:

```
bios = ovmf          machine = q35          (no efidisk0 entry)
scsi0 = local-lvm:vm-100-disk-1,iothread=1,size=8G,ssd=1
                     (no ipconfig0, no ide2 — the static IP is set inside the guest)
```

A config matching `haproxy-node.tf` declares a cloud-init `initialization`
block. Importing the live VM would leave Terraform wanting to attach a
cloud-init drive and reconcile the disk on the VM that serves
`cluster.jdwlabs.com:6443` — a change class that can stop the VM. Rebuilding
also exercises the recovery path, which is the entire point.

The old VM is therefore left out of Terraform state. It is deleted by hand
after the soak period, not by `terraform destroy`.

## Preconditions — hard gates, all must pass

1. `haproxy_vms` is empty in the current tfvars, so no load balancer is under
   Terraform management yet:
   ```bash
   cd terraform && terraform state list | grep -c haproxy   # expect 0
   ```
2. The chosen VMID is free. Re-derive live — do not trust this document:
   ```bash
   # via the Proxmox API, or on any node:
   pvesh get /cluster/resources --type vm --output-format json | jq -r '.[].vmid' | sort -n
   ```
3. The chosen temporary IP is free. Confirm nothing answers *and* nothing holds
   an ARP entry for it:
   ```bash
   ping -c2 <temp-ip>            # expect no reply
   arp -n | grep <temp-ip>       # expect no entry
   ```
   A colliding address takes down the API endpoint, not just the new VM.
4. Placement avoids control-plane hosts. Control planes currently sit on pve2,
   pve3, and pve4, so a load balancer belongs on pve1 (or pve5) — one host
   failure must not remove a control-plane node *and* the load balancer.
5. The target node's `local` datastore advertises both `snippets` and
   `import` content types:
   ```bash
   pvesh get /nodes/<node>/storage --output-format json \
     | jq -r '.[] | select(.storage=="local") | .content'
   ```
6. Secrets are hydrated (`talops secrets status`), including the remote-state
   credentials — those are hydrated manually and are not managed by `talops`.

## Provision the replacement

1. Add one entry to `haproxy_vms` in the vaulted tfvars, using the *temporary*
   address from precondition 3 — not the production address:
   ```hcl
   haproxy_vms = [
     {
       node_name = "pve1"
       vm_name   = "haproxy-1"
       vmid      = 110
       cpu_cores = 2
       memory    = 1024
       disk_size = 10
       ip        = "<temp-ip>/24"
     },
   ]
   haproxy_ssh_public_key = "ssh-ed25519 AAAA... you@host"
   ```
2. Plan and read it in full. Expect exactly three resources added — an image
   import, a cloud-init snippet, and the VM — and **zero** changes to any
   existing VM:
   ```bash
   cd terraform
   terraform plan -out=tfplan
   terraform show tfplan | less
   ```
3. **Abort** if the plan proposes any change to VMID 100, to a `talos-cp-*` or
   `talos-worker-*` VM, or to `vllm-inference`. Nothing in this change should
   touch them.
4. Human applies: `terraform apply tfplan`.
5. Wait for cloud-init to finish (roughly a minute — package install runs on
   first boot), then confirm the service exists:
   ```bash
   ssh haproxy-admin@<temp-ip> 'cloud-init status --wait && systemctl is-enabled haproxy'
   ```

## Verify before cutover

Every command here targets the replacement by address, so nothing touches the
load balancer still serving production.

1. Confirm the new VM is the one Terraform built, and that the layers below it
   answer:
   ```bash
   talops haproxy status --host <temp-ip>
   ```
   Expect `source: terraform`, `ssh: ok`, and `service: active`. `source:
   unmanaged` means the address matches no `haproxy_vms` entry — you are
   pointed at the wrong host. Backends are expected to be absent at this point:
   cloud-init ships only the distro placeholder config.
2. Review the config that would be pushed, then push it. This is the same
   validated/rollback path reconcile uses — not a manual copy:
   ```bash
   talops haproxy plan  --host <temp-ip>
   talops haproxy apply --host <temp-ip>
   ```
3. Smoke-test the API through the new VM without changing DNS:
   ```bash
   curl -sk --resolve cluster.jdwlabs.com:6443:<temp-ip> \
     https://cluster.jdwlabs.com:6443/version
   ```
   Expect a Kubernetes version document. A TLS error naming the API server's
   cert is still a pass for reachability; a connection refused is not.
4. Confirm every backend is up before trusting the replacement with traffic:
   ```bash
   talops haproxy status --host <temp-ip>
   ```
   The `backendsUp` line must read `N/N`. Anything less means a backend the
   production load balancer is currently serving would go dark at cutover.

## Cut over

The blip is bounded by the stop/start plus ARP settle — seconds. Schedule it
in a quiet window; DNS, `talosconfig`, and kubeconfig never change.

1. Stop the old VM (do not delete it yet):
   ```bash
   pvesh create /nodes/pve1/qemu/100/status/stop
   ```
2. Change the `haproxy_vms` entry's `ip` to the production address
   (`192.168.1.199/24`), then plan and review. This is an in-place cloud-init
   change to the *new* VM only.
3. Human applies, then reboot the new VM so cloud-init re-applies the address.
4. Switch `haproxy_login_user` to the cloud-init admin user in the same tfvars
   edit — the new VM has no `root` login. Then reconcile so the pushed config
   matches the cluster's live topology.
5. Confirm end to end:
   ```bash
   talops haproxy status              # expect source: terraform, backendsUp: N/N
   kubectl get nodes                  # through cluster.jdwlabs.com
   talosctl -n <cp-ip> version        # Talos API through :50000
   curl -sI https://<an-ingress-host> # ingress through :80/:443
   ```
   `talops haproxy status` reporting `source: terraform` against the production
   address is the signal that this runbook has actually landed: until then the
   load balancer is still the hand-built one.

## Verify the UDP ingress rules

**Nothing in this runbook restores these rules, and no `talops` command deploys
them.** The renderer exists and nothing calls it, so the ruleset file and the
unit that loads it are still placed on the VM by hand. Read that first: the
checks below confirm the rules are present, and every one of them passes on a
VM where UDP ingress is completely down, because a rebuilt VM has no rules at
all. The procedure that places them is in
`scenarios/bedrock-external-access.md` — follow it before running these checks,
not after.

A rebuild restores HAProxy and stops. It does **not** restore the other thing
this VM is: the single external UDP ingress hop. The router forwards the whole
range `19132-19141` to `192.168.1.199` with no port rewrite, and an nftables
table on this host DNATs each external port to the NodePort that owns it. That
table lives only on the VM's disk, so a rebuild silently drops every published
UDP service while the API and ingress look completely healthy.

**The table is not called `udpnat` on the VM running today.** Checked live
2026-10-02: `haproxy-1` carries `table ip minecraft`, the hand-applied
predecessor, loaded at boot by the enabled unit `minecraft-udp-nat.service`
from `/etc/minecraft-udp-nat.nft`. There is no `udpnat` table, no
`udpnat-rules.service` and no `/etc/nftables.d` on it — the migration in
`scenarios/bedrock-external-access.md` ("Migrating off the `minecraft` table")
has not been run. So which name to expect depends on which VM you are looking
at:

| VM | Table | Loaded by |
|---|---|---|
| `haproxy-1` as it stands | `ip minecraft` | `minecraft-udp-nat.service` |
| `haproxy-1` after the migration | `ip udpnat` | `udpnat-rules.service` |
| Any rebuilt VM | none, until the procedure is run — then `ip udpnat` | `udpnat-rules.service` |

A rebuild is the migration, for free: the new VM has neither table, so placing
`udpnat` on it per the procedure leaves nothing to delete and no loader to
hunt down.

Two constraints on this host shape how the rules load:

- `nftables.service` stays **disabled**. The distro's `/etc/nftables.conf`
  begins with `flush ruleset`, and this VM also runs Tailscale, whose rules sit
  in `table ip filter` and are installed at runtime. Enabling the service on a
  rebuilt VM drops them.
- The ruleset is therefore applied by a dedicated unit that loads only its own
  table, which is scoped so it can be replaced without touching anything else
  in the ruleset.

The file's contents are generated from cluster state by
`bootstrap/internal/udpnat` — it reads every Service carrying
`platform.jdwlabs.io/udp-external-port` and renders the DNAT and masquerade
rules. Publishing another server is an annotation on that Service; it is never
a new router rule and never a hand-edited line here.

After cutover, confirm all of these:

```bash
ssh haproxy-admin@192.168.1.199 'sysctl net.ipv4.ip_forward'      # expect: = 1
ssh haproxy-admin@192.168.1.199 'sudo nft list tables'            # expect udpnat or minecraft — exactly one
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip udpnat'   # whichever of the two
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip minecraft' #   the line above named
ssh haproxy-admin@192.168.1.199 'systemctl is-enabled udpnat-rules minecraft-udp-nat'
ssh haproxy-admin@192.168.1.199 'systemctl is-enabled nftables'   # expect: disabled
```

List the tables before reading one. `sudo nft list table ip udpnat` answers
`Error: No such file or directory` on today's healthy VM, where the rules are
in `minecraft` — that error means "no table by that name", not "UDP ingress is
down". Both tables present at once is a fault in its own right: they race, and
`scenarios/bedrock-external-access.md` explains why the loser stays wrong for
the life of a conntrack entry.

Of the two units, the one matching the table must read `enabled` and the other
`not-found` (or `disabled`). A table with no enabled loader works until the
next reboot and then does not — which, on a VM that only reboots when its host
does, is exactly when nobody is looking at UDP.

`ip_forward` is the one that bites. DNAT sends packets to a NodePort on a
*different* host, so with forwarding off the VM accepts them and drops them —
no refusal, no log, and every other check here still passes. Cloud-init now
writes `/etc/sysctl.d/99-ip-forward.conf`, so a VM built from
`haproxy-node.tf` comes up with it set; a VM built before that, or one where
the drop-in was removed, does not. The same sysctl is what
`scenarios/tailscale-subnet-router.md` needs, and on the 2026-08-13 run the VM
shipped with it at `0` — until that setup it was off on this host, and the UDP
rules were relying on it as a side effect.

The table should carry one `udp dport <external> dnat to
<node-ip>:<nodeport>` line per published service. Neither table in
`nft list tables` means external UDP is down even though
`talops haproxy status` reads healthy, because the two are unrelated paths.

## Abort criteria

- The plan proposes changes to any VM other than the new load balancer.
- The temporary or production address answers ARP from an unexpected MAC.
- After cutover, any control-plane backend stays DOWN for more than a minute.

Rollback at any point before step 4 of the cutover: start VMID 100 again
(`pvesh create /nodes/pve1/qemu/100/status/start`). It still holds the
production address and its own config, so service returns without a
Terraform action.

## Post-checks and cleanup

- Soak for at least one full day, watching the API and ingress.
- Only then delete the old VM by hand. It is not in Terraform state, so
  `terraform destroy` will not remove it — and must not be used to try:
  ```bash
  pvesh delete /nodes/pve1/qemu/100
  ```
- Once `haproxy-0` is gone, the load balancer is reproducible from git plus the
  vault alone, and this runbook becomes the tested recovery path.

## Emergency rebuild: the original host is offline

Status: **NOT EXECUTED.** Written from the 2026-10-01 pve1 outage. What was
actually run is one read-only `terraform plan` against pve5 while pve1 was
down, which is where the first two rows of the table below come from. The
route this section recommends — `terraform state rm`, then a targeted plan —
was **never planned, let alone applied**: pve1 came back after a manual power
cycle before it was needed. Every "expect" below is a prediction. Read the plan
as if this document were wrong.

This is the last resort, not the first move. A power cycle brought pve1 back in
2h08m with the load balancer intact; a rebuild leaves you with a new VM that is
missing three things no command restores (below). Work through
`scenarios/proxmox-host-dark.md` first and come here only when the host is not
coming back in a time you can accept.

### What breaks, and why

Each of these stops the tested path above. All were observed or re-checked
live; the right-hand column is the workaround this section uses.

| What | Why it bites with the host offline | Workaround |
|---|---|---|
| `proxmox_endpoint` is pve1 (`https://192.168.1.200:8006/api2/json`) | Every Terraform call to Proxmox fails before planning anything | `-var 'proxmox_endpoint=https://192.168.1.204:8006/api2/json'` — any surviving node serves the whole cluster's API. Worked on 2026-10-01 |
| `-target` on the new VM still plans operations on pve1 | The VM instances depend on the image and snippet resources as whole resources, so a target pulls in every instance of both. The plan read `4 to add, 1 to change, 1 to destroy`, including an in-place update of `haproxy_cloud_image["pve1"]` and a replacement of `haproxy_cloud_init["haproxy-1"]`. Those cannot succeed on a dead node and block the apply | Remove the old VM's three resources from state — step 5. **Untested** |
| `haproxy_vms` rejects two entries with the same `ip` | `terraform/variables.tf` validates distinct addresses, so old and new cannot both be declared on `192.168.1.199/24` | Replace the entry instead of adding one — step 4 |
| VMID 110 stays taken | VMIDs are cluster-wide, and `/etc/pve/nodes/pve1/qemu-server/110.conf` still exists while the node is merely offline | New VMID and name. 113 was free on 2026-10-02; re-derive |
| VM 110 has `onboot: 1` on `192.168.1.199` | The moment the dead host is powered on, the old VM boots and answers the same address as the replacement. On 2026-10-02 it started 21 s after the kernel did | Set `onboot: 0` from a surviving node **before** the host returns — step 2 |
| The provider needs an ssh-agent | Snippet upload goes over node SSH and the provider reads keys only from a running agent (`terraform/providers.tf`). Agent sessions on devbox have none: `ssh-add -l` fails with `Could not open a connection to your authentication agent` | The human who applies starts one — step 7 |
| No backup of VM 110 existed | Its disk is on pve1's `local-lvm`, so it cannot be started anywhere else, and during the outage `truenas-backup` held nothing for it | A nightly job exists as of 2026-10-02; the restore is untested. See [Backing up VM 110](#backing-up-vm-110) |

### Before you start

- **Terraform itself.** `terraform/providers.tf` requires `>= 1.16.3`, and
  devbox's system binary was `1.16.0` on 2026-10-02. Check `terraform version`
  and have a new enough binary to hand before step 5, not during it.
- **Remote state** lives on TrueNAS (`192.168.1.205:9000`), not on a Proxmox
  host, so it survives any single pve host. If TrueNAS is the thing that is
  down, nothing here can run.
- **Quorum.** Steps 2 and 3 need `/etc/pve` writable, which needs a quorate
  cluster — four of five votes with one host down:
  ```bash
  ssh root@192.168.1.204 'pvecm status | grep -E "Total votes|Quorate"'
  ```
- **The cluster API, without the load balancer.** kubeconfig points at
  `https://192.168.1.199:6443`, which is the thing that is down. The control
  planes answer directly, and their certificates cover their own addresses:
  ```bash
  kubectl --server=https://192.168.1.241:6443 get nodes   # or .98, .125
  ```

Who runs what: steps 1, 3 and 6 are read-only and an agent can run them. Steps
2, 4, 5, 7 and 8 change a hypervisor, the vault, Terraform state or a live
host, and are a human's. Re-joining the tailnet (after step 8) is a human's
*in their own terminal*, for a different reason — it mints a credential.

### Steps

1. **Confirm nothing holds the production address.** Preconditions 2-5 at the
   top of this runbook still apply, against the surviving node:
   ```bash
   ping -c2 192.168.1.199; arp -n | grep 192.168.1.199     # no reply, no entry
   ssh root@192.168.1.204 'pvesh get /cluster/resources --type vm --output-format json' \
     | jq -r '.[] | [.vmid, .name, .node, .status] | @tsv' | sort -n
   ```
   `jq` is not installed on the hypervisors — pipe to it locally, as above.
   Place the replacement on pve5: it is the only other host without a
   control plane. It is also already memory-overcommitted
   (`docs/devbox2-provisioning.md`, "pve5 is over-allocated"), so 1 GB more is
   an emergency placement, not a permanent home. Its `local` datastore
   advertised `import,iso,vztmpl,snippets,backup` on 2026-10-02, which
   satisfies precondition 5.

2. **Disarm the old VM.** Human. Do this first and do not skip it because the
   host "is dead anyway" — it returns the moment someone power-cycles it, and
   nobody will be thinking about this file when they do. `qm set` is proxied to
   the owning node and cannot reach it, so edit the config through the cluster
   filesystem, which every quorate node holds a writable copy of:
   ```bash
   ssh root@192.168.1.204 "sed -i 's/^onboot: 1$/onboot: 0/' /etc/pve/nodes/pve1/qemu-server/110.conf"
   ssh root@192.168.1.204 "grep '^onboot' /etc/pve/nodes/pve1/qemu-server/110.conf"   # expect: onboot: 0
   ```
   The file was confirmed readable from another node; the edit itself was not
   made on 2026-10-01. The returning node syncs `/etc/pve` from the quorate
   majority before it starts guests, so the change should reach it — should,
   because that too is untested here. Step 9 checks it rather than trusting it.

3. **Pick the VMID and name.** Anything free, from the list in step 1. This
   section uses `haproxy-2` and `113`.

4. **Replace the `haproxy_vms` entry** in the vaulted tfvars. Human. The old
   entry comes out and the new one goes in on the production address directly:
   ```hcl
   haproxy_vms = [
     {
       node_name = "pve5"
       vm_name   = "haproxy-2"
       vmid      = 113
       cpu_cores = 2
       memory    = 1024
       disk_size = 10
       ip        = "192.168.1.199/24"
     },
   ]
   ```
   There is no temporary address and no verify-before-cutover stage here. That
   stage exists to protect a load balancer that is still serving; there is not
   one. Leave `proxmox_endpoint` in tfvars alone — the override belongs on the
   command line, where it cannot be sealed into the vault and outlive the
   outage.

5. **Remove the old VM's resources from state.** Human — state is theirs, and
   this is the step nobody has run. Keep a copy first, on tmpfs because state
   holds secrets. The `&&` matters: no copy, no removal.
   ```bash
   cd terraform
   terraform state pull > "${XDG_RUNTIME_DIR:?}/pre-rm.tfstate" &&
   terraform state rm \
     'proxmox_virtual_environment_vm.haproxy["haproxy-1"]' \
     'proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-1"]' \
     'proxmox_virtual_environment_download_file.haproxy_cloud_image["pve1"]'
   ```
   `state rm` talks to the state backend only, never to Proxmox, so the dead
   node does not matter to it. It leaves VM 110, its snippet and its image
   exactly where they are and makes Terraform forget them — the same position
   `haproxy-0` was in, deliberately, for the original rebuild. Delete the
   tmpfs copy once the apply has landed.

6. **Plan, targeted, without refresh:**
   ```bash
   terraform plan -refresh=false \
     -var 'proxmox_endpoint=https://192.168.1.204:8006/api2/json' \
     -target='proxmox_virtual_environment_vm.haproxy["haproxy-2"]' \
     -out=tfplan
   terraform show tfplan | less
   ```
   Expect `3 to add, 0 to change, 0 to destroy` — `haproxy_cloud_image["pve5"]`,
   `haproxy_cloud_init["haproxy-2"]`, and the VM. **Abort** on any line that
   names pve1, `haproxy-1`, or any other VM. Both flags are load-bearing:
   - `-refresh=false`, because state still holds `talos-worker-01` and
     `devbox2` on the dead node, and refreshing them is a read that cannot be
     answered.
   - `-target`, because an untargeted plan also carries whatever is pending for
     those guests, and none of it can apply.

   An agent producing this plan for review adds `-lock=false`; the human's own
   plan takes the lock. The saved plan records the `-var`, so the apply needs
   no flags.

7. **Apply.** Human, in a shell with an agent holding a key `root@pve5`
   accepts:
   ```bash
   eval "$(ssh-agent -s)" && ssh-add ~/.ssh/<key>
   terraform apply tfplan
   rm tfplan        # a saved plan carries the provider's variables, token included
   ```

8. **Push the config and verify.** The address now belongs to a different host
   key, so clear the stale entry first or every SSH below fails as a key
   mismatch. Read the new fingerprints through the hypervisor's guest agent
   and check the scanned ones against them before trusting the scan — the
   address has just changed hands, which is the moment a wrong answer would go
   unnoticed. All three key types, because `talops` may negotiate any of them
   (`docs/host-addressing.md`, "SSH host keys are part of this"), so all three
   are appended and all three must match:
   ```bash
   ssh root@192.168.1.204 "qm guest exec 113 -- sh -c 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf \$f; done'"
   ssh-keygen -R 192.168.1.199
   ssh-keyscan -t rsa,ecdsa,ed25519 192.168.1.199 > "$XDG_RUNTIME_DIR/haproxy-2.keys"
   ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-2.keys"      # every line must match one above
   cat "$XDG_RUNTIME_DIR/haproxy-2.keys" >> ~/.ssh/known_hosts
   ssh haproxy-admin@192.168.1.199 'cloud-init status --wait && systemctl is-enabled haproxy'
   talops haproxy plan
   talops haproxy apply        # human
   talops haproxy status
   kubectl get nodes           # through 192.168.1.199 again, no --server
   ```
   `talops haproxy` renders from the recorded cluster state, not from live
   Proxmox discovery (`bootstrap/internal/app/haproxy.go`), so it does not need
   the dead node's API and `proxmox_endpoint` does not matter to it. For the
   same reason the rendered config still lists the worker on the dead host, and
   HAProxy marks it down: `backendsUp` reads one short of `N/N` on each ingress
   backend until the host returns. All three control-plane backends must be up.
   Not run in this configuration.

### What the replacement still lacks

`talops haproxy status` reads healthy with all three of these missing.

- **UDP ingress.** No table, no unit. Place `udpnat` per
  `scenarios/bedrock-external-access.md`, then run the checks in
  [Verify the UDP ingress rules](#verify-the-udp-ingress-rules).
- **The Tailscale subnet router.** Off-LAN `kubectl`/`talosctl` and the
  `192.168.1.0/24` route both ran on VM 110. Joining the tailnet mints a
  credential, so it is run by a human in a terminal outside any agent session —
  never through an agent's shell, which would write it to a transcript. Then
  re-approve the route. Steps: `docs/tailscale-subnet-router.md`, "Rebuild
  parity". The old node entry in the tailnet belongs to a VM that may yet
  return; leave it until step 9 is settled.
- **Possibly the gateway's port forward.** The `bedrock-udp` service is
  attached to a *device*, and `scenarios/bedrock-external-access.md` says in
  its Step 3 that the attachment is keyed on the MAC — `bc:24:11:1e:cd:65`,
  which is VM 110's ("The gateway's device names are misleading" has the device
  table). `haproxy-node.tf` does not pin a MAC, so the replacement gets a new
  one. Whether the forward follows the address or stays with the old MAC has
  **not been tested**. Check 3 of that runbook's Step 4, from off-network, is
  the only test; if it fails with checks 1 and 2 passing, re-attach the service
  to the new device.

The LAN DNS resolver (`dnsmasq`) is not on this list: cloud-init ships it.

### When the dead host returns

9. **Check VM 110 before anything else.** It must be stopped:
   ```bash
   ssh root@192.168.1.200 'qm list'
   ```
   If it is running, the edit in step 2 did not hold and two hosts are
   answering `192.168.1.199`. Stop it — `ssh root@192.168.1.200 'qm stop 110'`,
   human — then confirm which MAC the LAN now has for the address
   (`arp -n | grep 192.168.1.199`) before trusting the API again.
10. **Decide which VM stays.** Keeping `haproxy-2` means deleting VM 110 by
    hand after a soak — `pvesh delete /nodes/pve1/qemu/110`, never
    `terraform destroy`, which no longer knows it — plus the two files
    Terraform also forgot, or a later load balancer declared on pve1 will
    collide with them:
    ```
    /var/lib/vz/snippets/haproxy-1-cloud-init.yaml
    /var/lib/vz/import/ubuntu-24.04-server-cloudimg-amd64-haproxy.qcow2
    ```
    Moving back to pve1 is a second rebuild, by the tested path at the top of
    this runbook, with `haproxy-2` in the role `haproxy-0` played.
11. Everything else the returning host needs is in
    `scenarios/proxmox-host-dark.md`, "After the host is back".

## Backing up VM 110

**Decision: back it up. The job was created on 2026-10-02; a restore from it
has never been tested.**

During the outage the cluster had one backup job, for VMID 111 only, and
`truenas-backup` held seven nightly archives of VM 111 and one stray archive
of a VM 1000 that no longer exists. Nothing covered VM 110. Its disk is on
pve1's `local-lvm`, so with pve1 down there was no copy of it anywhere.

The job mirrors the existing one's shape, offset from its 02:00 slot so the
two do not write to the NAS at once. This is what was run, kept here for the
next VM that needs one:

```bash
pvesh create /cluster/backup \
  --vmid 110 \
  --storage truenas-backup \
  --schedule 01:30 \
  --mode snapshot \
  --compress zstd \
  --prune-backups keep-daily=7 \
  --enabled 1 \
  --comment "haproxy-1 nightly backup, scenarios/haproxy-vm-rebuild.md"
```

Read back the same day with `pvesh get /cluster/backup --output-format json`:
a second job, `vmid 110`, schedule `01:30`, `snapshot`, `zstd`,
`keep-daily=7`, on `truenas-backup`, enabled. **A job is not an archive.**
`pvesm list truenas-backup` showed nothing for VM 110 at that point — the
first one is written by the first 01:30 run. Check that it appeared before
counting on it, and until a restore has been drilled treat it as a copy of the
disk, not as a recovery path.

The trade-off:

- **For.** A restore brings back what a rebuild cannot: the UDP ruleset and
  its unit, the tailnet identity (no new credential to mint), `haproxy.cfg` as
  deployed, and the same MAC, which sidesteps the gateway question above. It
  skips steps 4-7 entirely — no state surgery, no ssh-agent, no Terraform
  version to fix first. The disk is 10 GB, so seven archives cost little next
  to VM 111's 40-48 GB each.
- **Against.** A restored VM is a second way to get a load balancer, and it
  competes with the first. While pve1's config exists it lands under a new
  VMID, unknown to Terraform — and `talops haproxy status` will not say so: it
  matches on address, finds the `haproxy-1` entry and VMID 110 in state, and
  reports `source: terraform` for a VM Terraform did not build. The archive
  holds the tailnet node key and the SSH host keys at rest on the NAS. And it
  depends on TrueNAS — though so does Terraform state, so the rebuild path is
  no more independent of it.

Backup wins because it shortens the outage that actually happened, and the
rebuild stays the path for a VM that is wrong rather than merely gone.

The restore has **not been tested**, and two traps are already visible:

- The archive's config still says `cicustom: user=local:snippets/haproxy-1-cloud-init.yaml`.
  That file exists only on pve1's `local` datastore. A VM restored onto another
  node is expected to refuse to start until the snippet is copied there or the
  `cicustom` line is removed.
- It restores `onboot: 1` and the same MAC and address as VM 110. Step 2 above
  matters more after a restore than after a rebuild, not less: two VMs with one
  MAC is worse than two with one address.

Drill the restore onto pve5 with the network device disconnected before
relying on it.

## Known follow-ups

- ~~`haproxy_login_user` is still `root` in tfvars while cloud-init creates
  `haproxy-admin`.~~ Done as part of the 2026-08-09 cutover — vaulted tfvars
  now carries `haproxy_login_user = "haproxy-admin"`.
- A single load balancer remains a single point of failure for the API and all
  ingress. The `haproxy_vms` list shape is what a keepalived VIP pair needs;
  adding the peer is a separate, scheduled change. The 2026-10-01 outage is
  what that costs: 2h08m with no API endpoint, no ingress and no external UDP.
- The emergency rebuild above is unexecuted, and its `terraform state rm` step
  is unplanned as well. Until it is drilled, the tested recovery for a dead
  host is getting the host back.
- `proxmox_endpoint` names one host, and it is the host the load balancer
  lives on — so the outage that needs a rebuild is the one that breaks
  Terraform. The command-line override works; making the endpoint survive a
  single host is not done. `docs/host-addressing.md` has the note.
- VM 110's nightly backup job exists as of 2026-10-02, and nothing has been
  restored from it. The drill is described in
  [Backing up VM 110](#backing-up-vm-110).
- `haproxy-1` still carries the hand-applied `minecraft` table rather than
  `udpnat`. Harmless while it runs; it is why the UDP checks above name two
  tables.
- The hand-built load balancer has no `socat`, so `talops haproxy status`
  reports backend health as unread against it. Cloud-init installs it, so the
  gap closes with the replacement — no action needed on the old VM.
- No `talops` command deploys the UDP ingress rules, so a rebuild brings the VM
  back with HAProxy healthy and every published UDP server dark until the
  procedure in `scenarios/bedrock-external-access.md` is run by hand. Whatever
  wires it up must treat a render error as "leave the live ruleset alone" —
  writing an empty ruleset would take every published server offline to fix one
  bad annotation.
