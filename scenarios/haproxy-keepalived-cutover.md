# Runbook: Cut over to the keepalived HAProxy pair

Status: **NOT EXECUTED.** Written against the code, a two-container keepalived
rehearsal and the pinned provider source; no step below has been run against
the live cluster. Every `terraform apply`, `qm` and `talops haproxy apply` here
is run by a human.

## Why

`haproxy-1` on pve1 is the only load balancer. When pve1 died on 2026-10-01 the
Kubernetes API, the Talos API and all ingress were dark for 2h08m while the
cluster behind them was healthy.

This runbook adds `haproxy-2` on pve5 and turns `192.168.1.199` into a virtual
address that keepalived moves to whichever instance survives. DNS, kubeconfig,
`talosconfig` and LAN clients keep pointing at `.199` and never change.
`haproxy-1` gives up `.199` as its own address and takes a new static one.

Design and rationale: `docs/haproxy-vm-provisioning.md` §5.5.

## What you end up with

| | haproxy-1 | haproxy-2 |
|---|---|---|
| Proxmox host | pve1 | pve5 |
| VMID | 110 | 113 |
| Own address | `<A>` (new) | `<B>` (new) |
| `vrrp_priority` | 150 | 100 |
| Holds `192.168.1.199` | when it is the VRRP master | when it is the VRRP master |

Neither instance preempts: the address stays where it is when a failed
instance returns, and there is no automatic fail-back. That is a decision, not
a default left in place. This runbook finishes with `haproxy-2` holding it.

What rides along, and what does not:

| Service | After cutover | Consequence while pve1 is down |
|---|---|---|
| Kubernetes API, Talos API, HTTP(S) ingress | Follows the virtual address | None beyond the failover itself (a few seconds, open connections reset) |
| LAN DNS (`dnsmasq`, `.199:53`) | Follows it. Both instances run the same config; `bind-dynamic` opens the socket when the address arrives | None |
| Bedrock UDP from the LAN (`.199:19132`) | Follows it once `haproxy-2` carries the UDP rules (step 8) | None. Existing sessions drop — NAT state is not shared |
| Bedrock UDP from the internet | **Stays on `haproxy-1`.** The gateway's forward is attached to `haproxy-1`'s MAC, not to `.199` | External Bedrock is down until pve1 returns. No worse than today |
| Tailscale subnet router | **Stays on `haproxy-1`.** | Tailnet clients lose the `192.168.1.0/24` route until pve1 returns. Hosts that are tailnet nodes themselves stay reachable |

Joining `haproxy-2` to the tailnet as a second subnet router would close the
last row, and moving the gateway forward onto a VRRP virtual MAC would close
the one above it. Both are tracked separately and are not part of this
runbook; the first mints a credential and is always a human step in a terminal
outside any agent session (`scenarios/tailscale-subnet-router.md`).

## The two traps this sequence is built around

**1. A plan targeted at one instance still replaces the other's snippet.**
`-target` follows dependencies between *resources*, not instances. Targeting
`proxmox_virtual_environment_vm.haproxy["haproxy-2"]` pulls in every instance
of `proxmox_virtual_environment_file.haproxy_cloud_init`, so step 5 also
replaces `haproxy-1`'s snippet. Nothing happens to `haproxy-1` at that moment.
At its next boot it gets a new cloud-init instance-id and re-runs cloud-init
with the group's user-data (`terraform/README.md`, "The part the guards do not
fix").

That re-run is wanted — it is how `haproxy-1` gets keepalived — but only once
its address has moved. Between step 5 and step 11 the snippet says "group
member at `<A>`" while the VM's network config still says `.199`. keepalived
started in that state deletes `.199` from the interface: it treats a configured
virtual address as a leftover and removes it (reproduced in the rehearsal).
The cloud-init therefore installs a systemd `ExecCondition` that skips
keepalived unless the instance's own address is configured, so an unplanned
reboot of `haproxy-1` in that window brings it back exactly as it is today.
Keep the window short anyway.

**2. A new instance must not claim the address before it can serve.** A fresh
VM runs the distro's placeholder `haproxy.cfg` and hears no VRRP peer. The
keepalived health check only passes while HAProxy listens on `:6443`, so it
stays out of the election until the real config is pushed. Once it *is* pushed
the instance becomes eligible — so step 7 stops keepalived first, and step 10
starts it at the moment `haproxy-1` lets go.

## Preconditions — hard gates, all must pass

1. The change is merged and `talops` is rebuilt from it.
2. `talops haproxy status` is clean against today's standalone load balancer
   (`ssh: ok`, `service: active`, `configDrift: false`, `backendsUp: N/N`).
3. **Choose the instance addresses.** `<A>` and `<B>` are not allocated by this
   repo. Both must be below `192.168.1.64`: the gateway's DHCP pool is
   observed to span roughly `.64`-`.253` (`docs/host-addressing.md`), so only
   an address under that floor cannot be leased to something else later. Both
   must also be unused, and several addresses down there are not — `.50`,
   `.56` and `.57` at least. Prove each candidate free from a LAN host —
   nothing answers, nothing holds a neighbour entry, and a duplicate-address
   probe gets no reply:
   ```bash
   for ip in <A> <B>; do
     ping -c2 -W1 "$ip"                       # expect: 100% packet loss
     ip neigh show "$ip"                      # expect: nothing, or FAILED
     sudo arping -D -c3 -I <lan-if> "$ip"     # expect: exit 0, "Received 0 response(s)"
   done
   ```
   Then check the gateway's device list for both. A silent host is not a free
   address: `192.168.1.198` looks free in this repo and is held by another
   device (`50:3d:d1:25:7e:56`). Record the two addresses on the ticket.
4. VMID 113 is free. Re-derive, do not trust this document:
   ```bash
   ssh root@pve5 "pvesh get /cluster/resources --type vm --output-format json" \
     | jq -r '.[].vmid' | sort -n
   ```
5. pve5 has room for 1 GiB more without leaning on KSM:
   ```bash
   ssh root@pve5 'free -g | sed -n 2p'        # "available" comfortably above 2
   ```
6. pve5's `local` datastore offers `snippets` and `import`:
   ```bash
   ssh root@pve5 "pvesh get /nodes/pve5/storage --output-format json" \
     | jq -r '.[] | select(.storage=="local") | .content'
   ```
7. No other VRRP group on the segment uses router id 51. Watch for ten seconds
   on `haproxy-1`; expect no packets:
   ```bash
   ssh haproxy-admin@192.168.1.199 'sudo timeout 10 tcpdump -ni eth0 -c5 vrrp'
   ```
   If something shows up, set `haproxy_vrrp_router_id` to an unused id in
   step 1.
8. Nothing allowlists `192.168.1.199` as a *source*. After cutover the
   instances originate traffic — health checks, and Tailscale's masqueraded
   subnet traffic — from `<A>` and `<B>`, not from `.199`.
9. Secrets are hydrated, including the remote-state credentials
   (`docs/secrets.md`).
10. A recent backup of VM 110 exists. `haproxy-1` re-runs cloud-init and
    changes address in this runbook, and it carries state nothing here
    rebuilds: the Tailscale node identity and the hand-placed UDP rules. The
    nightly job runs at 01:30 to `truenas-backup`; expect an entry from the
    last night:
    ```bash
    ssh root@pve1 'pvesm list truenas-backup --vmid 110 | tail -3'
    ```
11. A quiet window. Expect two interruptions of a few seconds each (steps 9-10
    and the failover test), and every open connection through the load
    balancer reset both times.
12. **You are on a LAN host**, with an address in `192.168.1.0/24` of its own
    — not a tailnet client reaching the LAN through the subnet route. Step 9
    shuts down `haproxy-1`, which is the subnet router, and the route stays
    down until step 12; a session that depends on it is cut off with the
    endpoint dark and no way to run step 10.
13. A running ssh-agent holding a key that `root@pve1` and `root@pve5` both
    accept. The provider uploads snippets and imports disks over node SSH and
    reads keys only from an agent (`terraform/providers.tf`), so without one
    the apply in step 5 fails partway:
    ```bash
    ssh-add -l || { eval "$(ssh-agent -s)" && ssh-add ~/.ssh/<key>; }
    ssh root@pve1 true && ssh root@pve5 true
    ```
14. `<A>` and `<B>` have no `known_hosts` entries, stale or otherwise:
    ```bash
    for ip in <A> <B>; do ssh-keygen -F "$ip"; done    # expect: no output
    ```
    The entries themselves cannot be made yet — neither address answers until
    its instance boots. Steps 6 and 12 add them, with
    `ssh-keyscan -t rsa,ecdsa,ed25519`: `talops` may negotiate any of the three
    key types, and an entry covering only some of them fails as a key mismatch
    (`docs/host-addressing.md`, "SSH host keys are part of this").

## Stage — nothing here touches production traffic

1. **Edit the vaulted tfvars**, in your own terminal (`talops secrets edit
   tfvars`). The VRRP secret is created in this edit and goes nowhere else —
   not a shell history, not an agent session:
   ```hcl
   haproxy_ip = "192.168.1.199"        # unchanged — now the virtual address

   haproxy_vms = [
     { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "<A>/24", vrrp_priority = 150 },
     { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "<B>/24", vrrp_priority = 100 },
   ]

   haproxy_vrrp_auth_pass = "<8 letters or digits>"
   ```
   Keep `haproxy-1`'s `cpu_cores`, `memory` and `disk_size` exactly as they are
   today; only `ip` changes and `vrrp_priority` is added.

   From this edit until step 11, `talops haproxy` without `--host` targets
   `<A>` and `<B>`, and `<A>` does not exist yet. Use `--host` as shown, and do
   not run `talops reconcile`.

2. **Plan the new instance only:**
   ```bash
   cd terraform
   terraform plan -target='proxmox_virtual_environment_vm.haproxy["haproxy-2"]' -out=tfplan-lb2
   terraform show tfplan-lb2 | less
   ```
3. **Read it. Expect `4 to add, 1 to change, 1 to destroy`, and exactly:**
   - create `proxmox_virtual_environment_download_file.haproxy_cloud_image["pve5"]`
   - create `proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-2"]`
   - create `proxmox_virtual_environment_vm.haproxy["haproxy-2"]`
   - **replace** `proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-1"]`
     — trap 1. The snippet content prints as `(sensitive value)` because it
     now carries the VRRP secret.
   - **update in place**
     `proxmox_virtual_environment_download_file.haproxy_cloud_image["pve1"]`
     — the target pulls in every instance of the image resource, the same way
     it does the snippet. A plan with this target was run on 2026-10-01 and
     carried this line (`scenarios/haproxy-vm-rebuild.md`, "What breaks, and
     why"). That record does not name the attribute; the dev VM's image had
     the same kind of pending diff, `overwrite` `true` → `false`
     (`terraform/README.md`). Whatever it is, it must read as an update — a
     replacement of this image is not this line.
4. **Abort** on anything else — in particular any line for
   `proxmox_virtual_environment_vm.haproxy["haproxy-1"]`, any `talos-*` VM,
   `dev_vm`, `devbox2` or `vllm-inference`, and anything in the destroy column
   other than the one snippet replacement.
5. Human applies: `terraform apply tfplan-lb2`. **The window from trap 1 opens
   here.**
6. Wait for first boot, then trust the new instance's host keys. Read the
   fingerprints through the hypervisor's guest agent and check the scanned
   ones against them before appending — the guest agent is installed by
   cloud-init, so the first command fails until that has got far enough;
   retry it:
   ```bash
   ssh root@pve5 "qm guest exec 113 -- sh -c 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf \$f; done'"
   ssh-keyscan -t rsa,ecdsa,ed25519 <B> > "$XDG_RUNTIME_DIR/haproxy-2.keys"
   ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-2.keys"      # every line must match one above
   cat "$XDG_RUNTIME_DIR/haproxy-2.keys" >> ~/.ssh/known_hosts
   ```
   Then confirm the guard and the health check landed and that the instance is
   *not* in the election:
   ```bash
   ssh haproxy-admin@<B> '
     cloud-init status --wait
     systemctl show keepalived -p ExecCondition | grep -c "<B>/"   # expect 1
     test -x /usr/local/sbin/haproxy-vip-ready && echo check-present
     ip -4 -o addr show eth0                                       # expect <B> only, no .199
     sysctl net.ipv4.ip_nonlocal_bind net.ipv4.ip_forward          # expect both = 1
   '
   ```
   `.199` on `haproxy-2` at this point means trap 2 failed. Stop keepalived on
   it immediately (`sudo systemctl disable --now keepalived`) and do not
   continue.
7. **Take `haproxy-2` out of the election, then give it the real config.** The
   order matters: the push is what makes it eligible.
   ```bash
   ssh haproxy-admin@<B> 'sudo systemctl disable --now keepalived'
   talops haproxy plan  --host <B>     # binds 192.168.1.199, not <B>
   talops haproxy apply --host <B>
   talops haproxy status --host <B>    # backendsUp must read N/N
   ```
   The config binds `.199` on a host that does not hold it; that works because
   cloud-init set `net.ipv4.ip_nonlocal_bind`. A `cannot bind socket` error from
   the push means the sysctl is missing — stop.
   Then prove the two instances can exchange VRRP on the addresses they
   will actually use, before either depends on it. Adverts are unicast IP
   protocol 112 between `<A>` and `<B>`; if anything on the path drops them,
   each instance hears nothing, both become master, and step 12 ends with
   `.199` on two hosts. `haproxy-1` does not have `<A>` yet, and a probe from
   `.199` would not exercise a filter keyed on address, so lend it `<A>` as a
   second address for the length of the test. It is not persisted and is gone
   after a reboot; precondition 3 proved it free.
   ```bash
   ssh haproxy-admin@192.168.1.199 'sudo ip addr add <A>/24 dev eth0'

   # <A> -> <B>
   ssh haproxy-admin@<B> 'sudo timeout 8 tcpdump -ni eth0 -c1 "ip proto 112 and src <A> and dst <B>"' &
   sleep 2
   ssh haproxy-admin@192.168.1.199 \
     "sudo python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_RAW, 112); s.bind((\"<A>\", 0)); s.sendto(bytes(8), (\"<B>\", 0))'"
   wait

   # <B> -> <A>
   ssh haproxy-admin@192.168.1.199 'sudo timeout 8 tcpdump -ni eth0 -c1 "ip proto 112 and src <B> and dst <A>"' &
   sleep 2
   ssh haproxy-admin@<B> \
     "sudo python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_RAW, 112).sendto(bytes(8), (\"<A>\", 0))'"
   wait

   ssh haproxy-admin@192.168.1.199 'sudo ip addr del <A>/24 dev eth0; ip -4 -o addr show eth0'   # expect .199 only
   ```
   Each `tcpdump` must print one packet. `0 packets captured` in either
   direction is a hard stop: find what filters protocol 112 between pve1 and
   pve5 before going any further.
8. **Place the UDP rules on `haproxy-2`** so LAN Bedrock clients follow the
   address. A new VM gets the `udpnat` table and its own loader unit
   (`scenarios/haproxy-vm-rebuild.md`, "Verify the UDP ingress rules") — not a
   copy of the hand-applied `minecraft` table and `minecraft-udp-nat.service`
   that `haproxy-1` still carries, which would only make a second host to
   migrate off them.

   Read what `haproxy-1` forwards today; `haproxy-2` must publish the same
   servers:
   ```bash
   ssh haproxy-admin@192.168.1.199 'sudo nft list table ip minecraft'
   ```
   Then follow `scenarios/bedrock-external-access.md`, "Placing the nftables
   rules on the HAProxy VM", steps 1-4, on `<B>`: one `dnat` and one
   `masquerade` line per rule in that output, written to
   `/etc/nftables.d/udpnat.nft` and loaded by `udpnat-rules.service`. Confirm:
   ```bash
   ssh haproxy-admin@<B> 'sudo nft list table ip udpnat; systemctl is-enabled udpnat-rules nftables; sysctl net.ipv4.ip_forward'
   ```
   Expect the same `dnat` lines as `haproxy-1`, then `enabled`, `disabled`
   (`nftables.service` flushes the whole ruleset and stays off on these VMs)
   and `net.ipv4.ip_forward = 1`.

   The rules match on port, not on destination address, so they are inert on
   an instance that is not receiving the traffic. Until `haproxy-1` is
   migrated the two instances hold the same rules under different table names,
   and a change to them is made on both. Never load both tables on one
   instance: which one wins is undefined
   (`scenarios/bedrock-external-access.md`, "Migrating off the `minecraft`
   table").

## Cut over — the endpoint is dark from step 9 until step 10 completes

9. **Stop `haproxy-1`.** Graceful, and confirmed stopped before going on, so
   two hosts never own `.199` at once:
   ```bash
   ssh root@pve1 'qm shutdown 110 && qm wait 110 -timeout 120; qm status 110'   # expect: stopped
   ```
10. **Let `haproxy-2` take the address:**
    ```bash
    ssh haproxy-admin@<B> 'sudo systemctl enable --now keepalived; sleep 6; ip -4 -o addr show eth0'
    ```
    The `sleep` is there because the address does not arrive at once:
    keepalived first needs the health check to pass (it runs every 2 s), then
    waits out the master-down interval — three missed adverts of 1 s, plus
    skew — before deciding no peer holds the address. A look any sooner shows
    `<B>` alone and reads as a failure. Expect `192.168.1.199/24 ... secondary`;
    if it is not there yet, run the `ip` command again before concluding
    anything. Then prove the endpoint from the LAN, not from the instance:
    ```bash
    kubectl get --raw /version
    talosctl -e 192.168.1.199 -n <cp-ip> version
    curl -skI https://<an-ingress-host>/ | head -1
    dig +short @192.168.1.199 cluster.jdwlabs.com     # expect 192.168.1.199
    ```
    **Rollback from here:** `sudo systemctl disable --now keepalived` on
    `haproxy-2`, then `ssh root@pve1 'qm start 110'`. `haproxy-1` re-runs
    cloud-init on that boot (trap 1) but its network config is unchanged, so
    it returns at `.199` with keepalived skipped. Its SSH host key will have
    changed — see step 12.
11. **Move `haproxy-1` to its own address.** Plan after the shutdown, not
    before, so the plan sees the VM as it is:
    ```bash
    terraform plan -target='proxmox_virtual_environment_vm.haproxy["haproxy-1"]' -out=tfplan-lb1
    terraform show tfplan-lb1 | less
    ```
    Expect **0 to add, 1 to change, 0 to destroy**: `haproxy-1`'s
    `initialization.ip_config.ipv4.address` `192.168.1.199/24` → `<A>/24`, and
    possibly `started` `false` → `true`. `2 to change` is also acceptable if the
    second is the in-place update of
    `proxmox_virtual_environment_download_file.haproxy_cloud_image["pve1"]`
    from step 3: the target pulls that resource in again, and whether step 5's
    apply cleared its diff has not been observed. Abort on any replacement, or
    any other resource. Human applies: `terraform apply tfplan-lb1`. **The
    window from trap 1 closes here.**

    The provider regenerates the cloud-init drive and restarts a VM whose
    `initialization` changed (`reboot_after_update` defaults to true; read in
    the provider source at the pinned version, not yet observed here). Confirm
    rather than assume, and start it by hand if it is still down:
    ```bash
    ssh root@pve1 'qm status 110 || true; qm config 110 | grep ipconfig0'   # expect ip=<A>/24
    ssh root@pve1 'qm start 110'                                           # only if stopped
    ```
12. **`haproxy-1` returns as a backup.** It re-runs cloud-init on this boot:
    new address, keepalived installed, and — because cloud-init treats it as a
    new instance — new SSH host keys. Trust them the way step 6 did, checked
    against the guest agent's reading:
    ```bash
    ssh-keygen -R 192.168.1.199        # the virtual address answers as whichever instance holds it
    ssh root@pve1 "qm guest exec 110 -- sh -c 'for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf \$f; done'"
    ssh-keyscan -t rsa,ecdsa,ed25519 <A> > "$XDG_RUNTIME_DIR/haproxy-1.keys"
    ssh-keygen -lf "$XDG_RUNTIME_DIR/haproxy-1.keys"      # every line must match one above
    cat "$XDG_RUNTIME_DIR/haproxy-1.keys" >> ~/.ssh/known_hosts
    ssh haproxy-admin@<A> '
      cloud-init status --wait
      ip -4 -o addr show eth0          # expect <A> only
      systemctl is-active haproxy keepalived dnsmasq minecraft-udp-nat tailscaled
      sudo tailscale status | head -3
    '
    ```
    `.199` in that `ip` output has two causes with opposite responses, so
    look at `haproxy-2` before touching anything:
    ```bash
    ssh haproxy-admin@<B> '
      ip -4 -o addr show eth0
      systemctl is-active keepalived haproxy
      ss -Hltn "sport = :6443" | wc -l      # 1 or more: HAProxy is serving
    '
    ```
    - **`haproxy-2` holds `.199` too, with both units active and the listener
      present** — the instances cannot hear each other and both are master.
      `haproxy-2` can carry the endpoint alone, so stop keepalived on
      `haproxy-1` (`sudo systemctl stop keepalived`), then compare the two
      `keepalived.conf` files and re-run the protocol 112 check from step 7.
    - **`haproxy-2` does not hold `.199`** — this is not a split. `haproxy-2`
      dropped out after step 10 and `haproxy-1` took over as designed. It is
      now the only holder: **leave its keepalived running** and find out why
      `haproxy-2` left (`journalctl -u keepalived -n 50` on it).
    - **`haproxy-2` holds `.199` but is not serving** — stop keepalived on
      `haproxy-2`, not on `haproxy-1`.

    If `haproxy-1` does not come back, the endpoint is unaffected:
    `haproxy-2` is serving. Fix or rebuild `haproxy-1` at leisure.

## Verify

13. The pair, in one call:
    ```bash
    talops haproxy status
    ```
    Expect `holder: haproxy-2`, both rows `ssh: ok`, `service: active`,
    `keepalived: active`, `configDrift: false`, `backendsUp: N/N`, and no
    `error:` line. Then `talops haproxy apply` — expect `0 changes pushed`.
14. **Failover test — this is the exit criterion.** Watch from a LAN host in
    one terminal:
    ```bash
    while :; do date +%T; curl -sk -m1 -o /dev/null -w '%{http_code}\n' https://192.168.1.199:6443/version; sleep 0.5; done
    ```
    and in another, take the holder's host out from under it the way an outage
    would:
    ```bash
    ssh root@pve5 'qm stop 113'
    ```
    Expect the loop to miss for under five seconds, then
    `talops haproxy status` to report `holder: haproxy-1` and fail with
    `instance_unreachable` for `haproxy-2`. Start it again
    (`ssh root@pve5 'qm start 113'`) and confirm the holder is **still**
    `haproxy-1` — no preemption — and the report is clean.
15. External paths that do not follow the address:
    - Bedrock from outside the LAN (`scenarios/bedrock-external-access.md`,
      Step 4). The gateway forward is attached to `haproxy-1`'s MAC, which now
      has `<A>` as its primary address. **How the gateway resolves that is
      unverified** — check it while `haproxy-1` holds the address and again
      while it does not. If it fails, re-attach the service to the same device
      in the gateway UI.
    - Tailscale: from an off-LAN tailnet client, reach a LAN-only address.
16. Record on the ticket: `<A>`, `<B>`, the two dark intervals as measured, and
    the status output from step 13.

## Close out

17. **Commit the re-sealed tfvars.** Step 1 re-sealed
    `terraform/terraform.tfvars.enc.yaml`, and so far that change exists only
    in this checkout. Every other checkout still hydrates the standalone
    definition from `main`, and a plan made from one proposes destroying
    `haproxy-2` and moving `haproxy-1` back to `.199`. Land it through a pull
    request, never a direct push to `main`:
    ```bash
    git status --short terraform/        # expect terraform.tfvars.enc.yaml, modified
    git switch -c <branch>
    git add terraform/terraform.tfvars.enc.yaml
    git commit -m "chore(secrets): declare the HAProxy keepalived pair"
    git push -u origin HEAD
    ```
    Not earlier than this: merged before the cutover, `main` would describe a
    pair that does not exist, and `talops haproxy` from any checkout would
    target an `<A>` nothing answers on.
18. **Update the runbooks this cutover makes wrong.** Each of these describes
    one load balancer on pve1 at `192.168.1.199`. Until they are corrected, the
    next person to open one during an outage is following instructions for a
    system that is gone. One pull request, with step 17's or after it:
    - [ ] `scenarios/haproxy-vm-rebuild.md`, "Emergency rebuild: the original
      host is offline" — losing pve1 no longer darkens the endpoint, so the
      section's premise and urgency go. `haproxy-2` and VMID 113 are taken. A
      replacement is a group member at its own address, not a VM on `.199`,
      so step 4's tfvars entry, step 8's host-key and SSH commands against
      `.199`, and the `onboot` disarm in step 2 (written for an old VM
      returning on the production address) all change. The status header and
      the single-point-of-failure follow-up go with them, and VM 113 needs
      the backup decision VM 110 got.
    - [ ] `scenarios/proxmox-host-dark.md` — pve1 down is no longer "no API
      endpoint": the `--server` workaround, check 1 (disarm VM 110) and the
      load-balancer entry under "Rebuild what the host carried" apply only
      when both instances are gone. What pve1 still takes with it is external
      UDP and the Tailscale route; what pve5 now takes is the pair's
      redundancy. The post-recovery checks SSH to `.199`, expect
      `source: terraform` from a standalone report and look for the
      `minecraft` table — they become per-instance, at `<A>` and `<B>`, and
      the returned instance is expected back as a backup, not as the holder.
    - [ ] `scenarios/bedrock-external-access.md` — the path diagram, the Step 1
      and device tables and Step 3 give the HAProxy VM as `192.168.1.199`;
      `haproxy-1` is now `<A>` with the same MAC, and step 15's result says
      what the gateway did with that. "Placing the nftables rules" and every
      `ssh haproxy-admin@192.168.1.199` become both instances by their own
      addresses. The `minecraft` migration is `haproxy-1` only. The
      single-point-of-failure gap narrows to external play, which still stops
      with pve1.

## Operating the pair afterwards

- **Never SSH or push to `192.168.1.199`.** It reaches whichever instance
  holds it, under that instance's host key. `talops haproxy` and `reconcile`
  address each instance at its own address; `--host <A|B>` narrows to one.
- **One instance down is a failing status, by decision.** `talops haproxy
  status` exits 1 with `instance_unreachable`: the endpoint is up and the pair
  has no redundancy. `reconcile` still runs — it pushes to the reachable
  instance, finishes its plan, then exits non-zero naming the instance that
  was skipped. Run `talops haproxy apply` once it is back; instances already
  current are skipped.
- **To move the address by hand**, stop keepalived on the holder
  (`sudo systemctl stop keepalived`), wait for the other to take it, start it
  again. It will not take the address back.
- **Rebooting an instance** re-runs cloud-init only if its snippet or network
  config changed since its last boot. Reboot the backup, never the holder.
- **Changing the cloud-init template** replaces both snippets in one apply
  (trap 1) and arms a re-run on each instance's next boot. Reboot them one at a
  time, backup first, moving the address in between.

## Roll back to a standalone load balancer

Only `haproxy-1` can be the standalone one without also moving Tailscale and
the gateway forward.

1. Make sure `haproxy-1` holds the address (move it by hand, above).
2. `ssh root@pve5 'qm shutdown 113'`.
3. Vaulted tfvars: remove the `haproxy-2` entry, set `haproxy-1`'s `ip` back to
   `192.168.1.199/24`, drop `vrrp_priority`. Leave `haproxy_vrrp_auth_pass`.
4. Plan scoped to the three load-balancer resources, so nothing else pending
   rides along, and read it. Planning changes nothing, so it is done while the
   endpoint is still up:
   ```bash
   terraform plan -out=tfplan-standalone \
     -target=proxmox_virtual_environment_vm.haproxy \
     -target=proxmox_virtual_environment_file.haproxy_cloud_init \
     -target=proxmox_virtual_environment_download_file.haproxy_cloud_image
   ```
   Expect `haproxy-2`'s VM, snippet and pve5 image destroyed, `haproxy-1`'s
   snippet replaced, and `haproxy-1`'s address changed in place.
5. On `haproxy-1`, only once the plan is accepted, and before the apply
   changes its address underneath keepalived:
   `sudo systemctl disable --now keepalived` — this drops `.199` and the
   endpoint goes dark until step 6 completes.
6. Human applies `tfplan-standalone`; confirm `haproxy-1` restarts at `.199`.
7. `talops haproxy status` — the standalone report, `host: 192.168.1.199`.
8. Commit the re-sealed tfvars through a pull request, as in step 17 of the
   cutover, and revert whatever step 18 changed.
