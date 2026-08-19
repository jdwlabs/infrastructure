# Runbook: Rebuild the HAProxy load-balancer VM

Status: **EXECUTED, 2026-08-09** (commit `568b368`). `haproxy-1` is the live
production load balancer at `192.168.1.199`; `haproxy-0` (VMID 100) was
stopped and is pending deletion after its soak period — re-confirm that
hasn't happened yet before assuming VMID 100 is free. Every `terraform apply`
in this runbook is executed by a human. The agent contract forbids autonomous
applies, and this VM carries the Kubernetes API, the Talos API, and all
HTTP(S) ingress. Kept here as the tested recovery path for the next rebuild,
not as a pending TODO.

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
`table ip udpnat` on this host DNATs each external port to the NodePort that
owns it. That table lives only on the VM's disk, so a rebuild silently drops
every published UDP service while the API and ingress look completely healthy.

Two constraints on this host shape how the rules load:

- `nftables.service` stays **disabled**. The distro's `/etc/nftables.conf`
  begins with `flush ruleset`, and this VM also runs Tailscale, whose rules sit
  in `table ip filter` and are installed at runtime. Enabling the service on a
  rebuilt VM drops them.
- The ruleset is therefore applied by a dedicated unit that loads only the
  `udpnat` table, which is scoped so it can be replaced without touching
  anything else in the ruleset.

The file's contents are generated from cluster state by
`bootstrap/internal/udpnat` — it reads every Service carrying
`platform.jdwlabs.io/udp-external-port` and renders the DNAT and masquerade
rules. Publishing another server is an annotation on that Service; it is never
a new router rule and never a hand-edited line here.

After cutover, confirm all three of these:

```bash
ssh haproxy-admin@192.168.1.199 'sysctl net.ipv4.ip_forward'      # expect: = 1
ssh haproxy-admin@192.168.1.199 'sudo nft list table ip udpnat'
ssh haproxy-admin@192.168.1.199 'systemctl is-enabled nftables'   # expect: disabled
```

`ip_forward` is the one that bites. DNAT sends packets to a NodePort on a
*different* host, so with forwarding off the VM accepts them and drops them —
no refusal, no log, and every other check here still passes. Cloud-init now
writes `/etc/sysctl.d/99-ip-forward.conf`, so a VM built from
`haproxy-node.tf` comes up with it set; a VM built before that, or one where
the drop-in was removed, does not. The same sysctl is what
`scenarios/tailscale-subnet-router.md` needs, and on the 2026-08-13 run the VM
shipped with it at `0` — until that setup it was off on this host, and the UDP
rules were relying on it as a side effect.

The `nft` output should carry one `udp dport <external> dnat to
<node-ip>:<nodeport>` line per published service. `No such file or directory`
means the table is absent — external UDP is down even though
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

## Known follow-ups

- ~~`haproxy_login_user` is still `root` in tfvars while cloud-init creates
  `haproxy-admin`.~~ Done as part of the 2026-08-09 cutover — vaulted tfvars
  now carries `haproxy_login_user = "haproxy-admin"`.
- A single load balancer remains a single point of failure for the API and all
  ingress. The `haproxy_vms` list shape is what a keepalived VIP pair needs;
  adding the peer is a separate, scheduled change.
- The hand-built load balancer has no `socat`, so `talops haproxy status`
  reports backend health as unread against it. Cloud-init installs it, so the
  gap closes with the replacement — no action needed on the old VM.
- No `talops` command deploys the UDP ingress rules, so a rebuild brings the VM
  back with HAProxy healthy and every published UDP server dark until the
  procedure in `scenarios/bedrock-external-access.md` is run by hand. Whatever
  wires it up must treat a render error as "leave the live ruleset alone" —
  writing an empty ruleset would take every published server offline to fix one
  bad annotation.
