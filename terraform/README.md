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
imported from it. That is a spurious in-place diff rather than a rebuild —
`import_from` is `ForceNew: false` and is only read when the disk is created,
so the update is a no-op for a live VM — but it is noise in every plan, and
noise is what let the real replacement above go unnoticed.

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

#### The case where the VM guard bites

`user_data_file_id` is not an opaque handle. Its value is
`<datastore>:snippets/<file_name>`, and `file_name` is derived from the VM
name variable — so **renaming a VM, or moving its snippet datastore, changes
the path, not just the resource's identity.** Terraform will destroy the old
snippet and create the new one, the guard will suppress the VM's side of it,
and the running VM's `cicustom` will keep pointing at a path that no longer
exists. Nothing fails at apply time. It fails at the next `qm start`.

So whenever `*_name` or `*_snippet_datastore` moves for a VM that is already
built, the snippet rename is not a self-contained change. Either rebuild the
VM deliberately:

```sh
terraform plan -target=proxmox_virtual_environment_vm.<vm> \
  -replace=proxmox_virtual_environment_vm.<vm> -out=tfplan
```

or, if the VM must survive, repoint it by hand after the apply and before it
next stops:

```sh
ssh root@<node> 'qm set <vmid> --cicustom user=<datastore>:snippets/<new-name>'
```

The guard is deliberately narrow: it protects against content churn, which is
constant, at the cost of not noticing a path change, which is rare and
deliberate. Renames are the one case that has to be driven, not planned.

For the images the guard restates what the attributes actually mean: the
checksum is a download-time integrity gate, not a continuously enforced
property, and nothing verifies it against the datastore afterwards. Rolling
an image is therefore a deliberate act, not a plan side effect: bump the URL
and checksum, then, for each image,

```sh
terraform plan -target=<address> -replace=<address> -out=tfplan
terraform show tfplan
terraform apply tfplan
```

`-replace` marks a resource for replacement; it does not narrow what else the
plan picks up. Without `-target` an image roll would carry every other pending
change with it — today that means the `haproxy-1` snippet replacement and the
dnsmasq rollout it arms, which is exactly the coupling the sequencing below
exists to prevent. Both flags, every time.

Existing VMs are unaffected by the re-download: `import_from` is
`ForceNew: false` and is read only when the disk is created, so the image is
copied into the VM's own disk once and never consulted again.

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
the result. The `.tfstate` extensions are load-bearing: `.gitignore` matches
them on `*.tfstate` but ignores nothing named `state-*.json`, and these files
hold the whole infrastructure state including secrets — so the same runbook
written with `.json` names puts them one `git add .` away from being
committed.

```sh
terraform state pull > state-backup.tfstate

python3 - <<'EOF'
import json, sys

d = json.load(open('state-backup.tfstate'))
hits = 0
for r in d['resources']:
    if r['type'] == 'proxmox_virtual_environment_file' and r['name'] == 'dev_vm_cloud_init':
        for i in r['instances']:
            a = i['attributes']['source_raw'][0]
            fixed = a['data'].replace('\r\n', '\n')
            if fixed != a['data']:
                a['data'] = fixed
                hits += 1

# Without this the script is a no-op that still bumps the serial and pushes
# byte-identical state — which reads as a successful repair and is not one.
if hits != 1:
    sys.exit(f'expected exactly 1 CRLF rewrite, made {hits}; check the resource address')

d['serial'] += 1
json.dump(d, open('state-fixed.tfstate', 'w'), indent=2)
EOF

terraform state push state-fixed.tfstate
terraform plan   # dev_vm_cloud_init must now be absent from the plan
```

If the plan is wrong and the push has to be undone, the backup's serial is now
behind the remote's and a plain push is refused. Force it — this is the one
place that flag is correct, because the backup is known-good state that was
deliberately superseded:

```sh
terraform state push -force state-backup.tfstate
```

Then shred both files; they are full state, and they do not belong in a
working tree any longer than the repair takes:

```sh
shred -u state-backup.tfstate state-fixed.tfstate
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
