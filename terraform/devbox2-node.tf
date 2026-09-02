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
}

output "devbox2_address" {
  description = "Provisioned lifeboat VM identity, for the restart-coordination runbook."
  value = {
    vmid = proxmox_virtual_environment_vm.devbox2.vm_id
    node = proxmox_virtual_environment_vm.devbox2.node_name
    ip   = split("/", var.devbox2_ip)[0]
  }
}
