# Terraform — Proxmox VM layer

This directory declares the Proxmox VMs (Talos control planes and workers, the
HAProxy load balancer, the GPU inference VM, devbox and devbox2), the cloud
images they import from, and the cloud-init snippets they boot with. State
lives on MinIO — `providers.tf` documents the backend and `docs/secrets.md` the
credentials.

Provisioning designs and runbooks live in `docs/`. This file covers one thing:
why the `lifecycle` blocks in here exist, and what they cost.

## The replacement traps

Two provider behaviours in `bpg/proxmox` will destroy running VMs during an
apply that looks routine. Both were live at once before the guards below
landed, and an untargeted plan read `6 to add, 2 to change, 6 to destroy` —
one of the six destroys being devbox, the machine the operator works from.

### Snippet content replaces the VM

`proxmox_virtual_environment_file.source_raw.data` is ForceNew. Any byte
difference replaces the snippet file, which makes its `id` unknown, which
makes `initialization[0].user_data_file_id` unknown on every VM that consumes
it — and that attribute is ForceNew too. So an edit to a cloud-init template
does not update a VM, it destroys and recreates it, losing the disk.

"Any byte difference" includes line endings. The snippets in state were
uploaded from a CRLF checkout; the templates render LF today. The content is
identical after normalising, and Terraform still planned a full rebuild of
devbox off the back of it. `.gitattributes` pins `*.tftpl` to LF so a CRLF
checkout cannot reintroduce the divergence, but it does not retroactively
correct state or the files already on the datastores.

### Image downloads replace themselves, then churn the VMs

`proxmox_virtual_environment_download_file` replaces itself when `checksum`,
`checksum_algorithm` or the observed `size` changes, and all three fire here:

- The three older image resources were applied before a checksum was added to
  their configuration, so state holds `null` and config holds a value.
- `overwrite` defaults to `true`, which makes the provider compare the
  datastore file's size against the URL's on every plan. The URL is a rolling
  pointer at the current Ubuntu point release, so each upstream respin makes
  every image resource plan as a replacement — indefinitely, and unprompted.

A replaced image also makes `disk.import_from` unknown on each VM that
imported from it. That plans as an in-place update rather than a replacement,
so it is not fatal, but it is a diff on a live VM whose apply-time behaviour
is not worth discovering by experiment.

## What is guarded, and why that is the right shape

Every VM that takes its cloud-init from a snippet — devbox, devbox2,
haproxy-1 — carries:

```hcl
lifecycle {
  ignore_changes = [initialization[0].user_data_file_id]
}
```

Every `download_file` carries `overwrite = false` and:

```hcl
lifecycle {
  ignore_changes = [checksum, checksum_algorithm]
}
```

`ignore_changes` on a nested block attribute is honoured — verified by plan,
not assumed: with the guard in place `dev_vm` drops from destroy-and-recreate
to a single in-place `memory` update. It also does not apply at resource
creation, so a genuinely new VM, or a rebuilt one, still gets whatever
cloud-init content and checksum are current at that time. What is suppressed
is only the proposal to reconcile an existing resource.

For the images the guard restates what the attributes actually mean: the
checksum is a download-time integrity gate, not a continuously enforced
property, and nothing verifies it against the datastore afterwards. Rolling
an image is therefore a deliberate act, not a plan side effect — bump the URL
and checksum, then `terraform apply -replace=<address>` for each image, and
expect a fresh download. Existing VMs are unaffected: `import_from` copies the
image into the VM's own disk at creation and never reads it again.

### The part the guards do not fix

Guarding the VM stops the *VM* being replaced. It does not make replacing a
*snippet* invisible to the guest.

PVE rebuilds the NoCloud seed ISO from the `cicustom` snippet on every VM
start, and derives the seed's `instance-id` as a SHA-1 over the user-data and
network-data. Replacing the snippet therefore hands the guest a new
`instance-id`, and cloud-init treats the next boot as a new instance and
re-runs its per-instance modules — package installs, user and SSH key setup,
`runcmd`. Not at apply time; at the next reboot, which is easy to miss because
the apply looks clean and the guest keeps running.

That is why snippet replacements are sequenced with the reboot they imply
rather than folded into an unrelated apply. Concretely: the pending
`haproxy-1` snippet change carries the LAN split-horizon DNS config, so
applying it arms a dnsmasq rollout on the production load balancer that will
fire whenever that VM next restarts. It belongs with that work
(`scenarios/lan-dns-resolver-deploy.md`), not with a memory resize.

## Clearing the devbox snippet's line-ending drift

devbox's snippet differs from the rendered template in line endings only. The
honest repair is to correct the record rather than rewrite the file, because
rewriting the file changes devbox's `instance-id` and re-runs cloud-init on
the next boot — which is the resize reboot, the one boot where the operator
most needs the box to come back exactly as it left.

Run from devbox2, not from devbox, and keep the backup until a plan confirms
the result:

```sh
terraform state pull > state-backup.json
python3 - <<'EOF'
import json
d = json.load(open('state-backup.json'))
for r in d['resources']:
    if r['type'] == 'proxmox_virtual_environment_file' and r['name'] == 'dev_vm_cloud_init':
        for i in r['instances']:
            s = i['attributes']['source_raw'][0]
            s['data'] = s['data'].replace('\r\n', '\n')
d['serial'] += 1
json.dump(d, open('state-fixed.json', 'w'), indent=2)
EOF
terraform state push state-fixed.json
terraform plan   # dev_vm_cloud_init must now be absent from the plan
```

This touches no Proxmox object. The file on the datastore keeps its CRLF bytes
until the next genuine template edit replaces it, at which point the
`instance-id` change happens alongside a change that was worth a reboot
anyway.

Two alternatives were considered and rejected:

- **Render CRLF from the template** (a `replace()` in the `.tftpl`) so the
  content matches state byte for byte. Zero risk and zero Proxmox mutation,
  but it contradicts the `.gitattributes` LF pin, has to be reverted later in
  a second change, and leaves a permanent wart in a file whose whole job is to
  be readable cloud-config.
- **Let the snippet be replaced.** Simplest, and it makes state and the
  datastore both correct — but it spends the `instance-id` change, and
  therefore a cloud-init re-run of `package_update`, the `users` module and
  `runcmd`, on the resize reboot of the operator's only workstation. The
  cloud-init here is idempotent day-0 config so it would most likely be
  uneventful, and "most likely uneventful" is the wrong standard for the boot
  you are relying on to come back.

## Applying the devbox memory reclaim

`dev_vm_memory` is 16384 in config and 32768 in state. The change is in-place,
but `balloon: 0` means the allocation is dedicated and only re-read when the
VM is fully stopped and started — a guest reboot will not pick it up.

Run this from devbox2 (192.168.1.57). Running it from devbox powers off the
machine mid-apply. `docs/devbox2-provisioning.md` §5 carries the full ordering
and the verification either side of it; the Terraform-specific part is:

1. Clear the snippet drift above and confirm `dev_vm_cloud_init` no longer
   appears in the plan.
2. Plan the resize scoped to the VM, and read it before applying:
   ```sh
   terraform plan -target=proxmox_virtual_environment_vm.dev_vm -out=tfplan
   terraform show tfplan    # expect: 0 to add, 2 to change, 0 to destroy
   terraform apply tfplan
   ```
   The two changes are `dev_vm`'s `memory.dedicated` 32768 -> 16384 and its
   image's `overwrite` true -> false. Anything else — in particular anything
   in the destroy column — means stop.
3. Stop and start devbox from pve5. `qm shutdown` rather than `qm stop`: the
   guest agent is available and this is the operator's own workstation.
   ```sh
   ssh root@pve5 'qm shutdown 111'
   ssh root@pve5 'qm start 111'
   ```

The `-target` is not caution for its own sake — it is what keeps the pending
`haproxy-1` snippet replacement, and the dnsmasq rollout it arms, out of an
apply that is only supposed to resize a VM.
