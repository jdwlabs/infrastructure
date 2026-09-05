# Lifeboat VM on pve1 — a deliberately small always-on workstation on a
# different physical host from devbox, so a pve5 restart doesn't leave the
# operator without a shell to drive it from. See
# docs/devbox2-provisioning.md for the capacity analysis behind the sizing.

# Separate download from the dev VM's: the "import" datastore is host-local, so
# the image has to be staged on devbox2's node, not devbox's. The URL and
# checksum are shared with the dev VM on purpose — one Ubuntu release to track,
# one checksum to rotate.
resource "proxmox_virtual_environment_download_file" "devbox2_cloud_image" {
  content_type       = "import"
  datastore_id       = var.devbox2_image_datastore
  node_name          = var.devbox2_node
  file_name          = "ubuntu-24.04-server-cloudimg-amd64-devbox2.qcow2"
  url                = var.dev_vm_cloud_image_url
  checksum           = var.dev_vm_cloud_image_checksum
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

resource "proxmox_virtual_environment_file" "devbox2_cloud_init" {
  content_type = "snippets"
  datastore_id = var.devbox2_snippet_datastore
  node_name    = var.devbox2_node

  source_raw {
    file_name = "${var.devbox2_name}-cloud-init.yaml"
    data = templatefile("${path.module}/templates/devbox2-cloud-init.yaml.tftpl", {
      hostname       = var.devbox2_name
      admin_user     = var.devbox2_user
      ssh_public_key = var.dev_vm_ssh_public_key
    })
  }
}

resource "proxmox_virtual_environment_vm" "devbox2" {
  name      = var.devbox2_name
  node_name = var.devbox2_node
  vm_id     = var.devbox2_id

  description = "Lifeboat VM: shell, tailnet and cluster admin tooling for driving host restarts while devbox is down"

  cpu {
    cores = var.devbox2_cores
    type  = "host"
  }

  memory {
    dedicated = var.devbox2_memory
  }

  disk {
    datastore_id = var.devbox2_storage_pool
    interface    = "scsi0"
    size         = var.devbox2_disk_size
    iothread     = true
    discard      = "on"
    import_from  = proxmox_virtual_environment_download_file.devbox2_cloud_image.id
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
    datastore_id = var.devbox2_storage_pool

    ip_config {
      ipv4 {
        address = var.devbox2_ip
        gateway = var.devbox2_gateway
      }
    }

    user_data_file_id = proxmox_virtual_environment_file.devbox2_cloud_init.id
  }

  # The whole point is being up when the other box isn't — including after the
  # host it lives on reboots.
  on_boot = true

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0"]
  tags            = ["dev", "lifeboat"]
  # Same ForceNew trap and same guard as the dev VM and the HAProxy VM: a
  # cloud-init template edit would otherwise destroy and rebuild this VM. It
  # matters more here than the sizing suggests — this is the lifeboat, and it
  # is worth least at exactly the moment it gets rebuilt, which is while the
  # operator is using it to drive a change to the other box. See the HAProxy
  # VM for the mechanism and the reboot caveat.
  lifecycle {
    ignore_changes = [initialization[0].user_data_file_id]
  }
}

output "devbox2_address" {
  description = "Provisioned lifeboat VM identity, for the restart-coordination runbook."
  value = {
    vmid = proxmox_virtual_environment_vm.devbox2.vm_id
    node = proxmox_virtual_environment_vm.devbox2.node_name
    ip   = split("/", var.devbox2_ip)[0]
  }
}
