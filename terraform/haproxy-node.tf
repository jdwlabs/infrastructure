# HAProxy load-balancer VM(s) fronting the Kubernetes API (6443), Talos API
# (50000), and HTTP(S) ingress. This is the day-0 path only: the VM shell, OS,
# packages, static address, and SSH access. The haproxy.cfg lifecycle stays
# with the generator that already reconciles backends when node membership or
# IPs change.
#
# Defaults to an empty list, so a checkout with no haproxy_vms in tfvars plans
# zero changes. The load balancer that currently fronts the control plane was
# built by hand and is deliberately NOT represented here — adopting it would
# put the live API endpoint one plan away from a cloud-init/EFI reconciliation
# it cannot satisfy in place. A replacement is provisioned alongside it and
# cut over, so the rebuild path is exercised rather than assumed.

# Per-node, because a download_file is scoped to one node's datastore. A
# distinct file_name keeps this from colliding with the GPU VM's identical
# image import if an operator ever places a load balancer on that same node.
resource "proxmox_virtual_environment_download_file" "haproxy_cloud_image" {
  for_each = toset([for vm in var.haproxy_vms : vm.node_name])

  content_type       = "import"
  datastore_id       = var.haproxy_image_datastore
  node_name          = each.value
  file_name          = "ubuntu-24.04-server-cloudimg-amd64-haproxy.qcow2"
  url                = var.haproxy_cloud_image_url
  checksum           = var.haproxy_cloud_image_checksum
  checksum_algorithm = "sha256"
  overwrite          = false

  # Create-time verification only. Neither a checksum edit nor an upstream
  # respin of this rolling URL should replace an image that VMs have already
  # copied from — under the provider's defaults both do, and that replacement
  # used to cascade into the VMs. terraform/README.md: "Image downloads
  # replace themselves", and how to roll an image deliberately.
  lifecycle {
    ignore_changes = [checksum, checksum_algorithm]
  }
}

# Snippets cannot live on an LVM-thin pool; they need a directory-backed
# datastore. The per-node `local` dir storage already advertises the snippets
# content type, which keeps a load-balancer rebuild independent of the NAS.
resource "proxmox_virtual_environment_file" "haproxy_cloud_init" {
  for_each = { for vm in var.haproxy_vms : vm.vm_name => vm }

  content_type = "snippets"
  datastore_id = var.haproxy_snippet_datastore
  node_name    = each.value.node_name

  source_raw {
    file_name = "${each.value.vm_name}-cloud-init.yaml"
    data = templatefile("${path.module}/templates/haproxy-cloud-init.yaml.tftpl", {
      hostname       = each.value.vm_name
      admin_user     = var.haproxy_admin_user
      ssh_public_key = var.haproxy_ssh_public_key
      # Single source of truth for the LAN split-horizon override — see the
      # file itself for why its values are hardcoded rather than templated.
      dnsmasq_config = trimspace(file("${path.module}/files/dnsmasq-jdwlabs-lan.conf"))
    })
  }
}

resource "proxmox_virtual_environment_vm" "haproxy" {
  for_each = { for vm in var.haproxy_vms : vm.vm_name => vm }

  name      = each.value.vm_name
  node_name = each.value.node_name
  vm_id     = each.value.vmid

  description = "HAProxy load balancer: Kubernetes API, Talos API, and ingress frontends"

  cpu {
    cores = each.value.cpu_cores
    type  = "host"
  }

  memory {
    dedicated = each.value.memory
  }

  # Left on the provider's SeaBIOS default rather than the OVMF/q35 pairing the
  # GPU VM needs for PCIe passthrough. An Ubuntu cloud image boots fine on
  # SeaBIOS, and skipping OVMF means no EFI disk to keep in sync.

  disk {
    datastore_id = var.storage_pool
    interface    = "scsi0"
    size         = each.value.disk_size
    iothread     = true
    discard      = "on"
    import_from  = proxmox_virtual_environment_download_file.haproxy_cloud_image[each.value.node_name].id
  }

  network_device {
    bridge = "vmbr0"
    model  = "virtio"
  }

  agent {
    enabled = true
    trim    = true
  }

  initialization {
    datastore_id = var.storage_pool

    # Static, never a DHCP lease. A control-plane address moving under a
    # running cluster has already caused an etcd outage here; the address the
    # API endpoint and every talosconfig resolve to must not be lease-dependent.
    ip_config {
      ipv4 {
        address = each.value.ip
        gateway = var.haproxy_gateway
      }
    }

    user_data_file_id = proxmox_virtual_environment_file.haproxy_cloud_init[each.value.vm_name].id
  }

  # The API endpoint and all ingress ride this VM: it must return unattended
  # after its Proxmox host reboots.
  on_boot = true

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0"]
  tags            = ["haproxy", "loadbalancer"]

  # user_data_file_id and the file resource behind it (source_raw.data) are
  # both ForceNew: true in the pinned v0.111.1 provider source
  # (proxmoxtf/resource/vm/vm.go's mkInitializationUserDataFileID and
  # proxmoxtf/resource/file.go's mkResourceVirtualEnvironmentFileSourceRaw*),
  # confirmed directly rather than inferred — the compiled schema alone
  # (`terraform providers schema -json`) doesn't expose ForceNew, only
  # nesting shape, which is where the [0] index below comes from
  # (`initialization` is schema.TypeList in that same source, i.e. a
  # list-nested block). Left unguarded, any edit to the cloud-init content —
  # including the dnsmasq config added alongside this comment — replaces the
  # snippet file, which changes its id, which replaces this VM: the live
  # production load balancer for the Kubernetes API, Talos API, and all
  # ingress, rebuilt from a cloud-init that deliberately ships no
  # haproxy.cfg. That is an outage, not a config push, and
  # `terraform plan`/`apply` would do it silently — CI here only runs
  # `terraform validate`, which cannot see it.
  #
  # This also matches reality for as long as the VM stays up: cloud-init only
  # runs per instance, so an edit to the snippet does not reach a running
  # guest (see "Day-0 is the gap" in docs/haproxy-vm-provisioning.md).
  # It does reach it across a reboot, though, and that is the part worth
  # spelling out: PVE rebuilds the NoCloud seed from this snippet on every VM
  # start and derives instance-id as a digest of the user-data and
  # network-data, so replacing the *file* gives the guest a new instance-id
  # and cloud-init re-runs its per-instance modules at the next boot. The
  # guard below stops the VM being destroyed; it does not make a snippet
  # replacement a no-op for the guest. Applying one is therefore scheduled
  # alongside the reboot it implies, not folded into an unrelated apply —
  # terraform/README.md has the sequencing.
  # ignore_changes does not apply at resource creation, so a genuinely new
  # VM (new vm_name/vmid — an HA peer, or the next full rebuild) still picks
  # up whatever cloud-init content is current at that time. For an already-live
  # VM there are two paths, and they differ in when the change lands, not in
  # whether it does: the SSH runbook
  # (scenarios/lan-dns-resolver-deploy.md is the current example) applies it
  # now and is the one to reach for; a `terraform apply` of the snippet applies
  # it at the VM's next boot, whenever that turns out to be. The second is only
  # safe if that boot is part of the plan.
  lifecycle {
    ignore_changes = [initialization[0].user_data_file_id]
  }
}

locals {
  haproxy_vms_by_name = { for vm in var.haproxy_vms : vm.vm_name => vm }
}

output "haproxy_vm_addresses" {
  description = "Provisioned HAProxy VMs keyed by name, for the config-push layer and the rebuild runbook."
  value = {
    for name, vm in proxmox_virtual_environment_vm.haproxy : name => {
      vmid = vm.vm_id
      node = vm.node_name
      ip   = split("/", local.haproxy_vms_by_name[name].ip)[0]
    }
  }
}
