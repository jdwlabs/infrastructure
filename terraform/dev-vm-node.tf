# Daily-driver dev VM on pve5 — SSH + VS Code Remote-SSH target for git/build/
# Claude Code sessions, off the Windows workstation. See
# docs/dev-vm-provisioning.md for the full design and phase plan. Disk lives
# on the NFS-backed datastore (var.dev_vm_storage_pool), not local/local-lvm,
# so the VM can `qm migrate --online` between Proxmox hosts with only RAM
# state to transfer.

# content_type "import": API-based import_from instead of the provider's
# node-SSH importdisk path, which cannot reach an ssh-agent from this
# workstation. Staged on "local" — the disk itself lands on the NFS
# datastore via the disk block's datastore_id below.
resource "proxmox_virtual_environment_download_file" "dev_vm_cloud_image" {
  content_type = "import"
  datastore_id = var.dev_vm_image_datastore
  node_name    = var.dev_vm_node
  file_name    = "ubuntu-24.04-server-cloudimg-amd64-devvm.qcow2"
  url          = var.dev_vm_cloud_image_url
}

# Snippets cannot live on an LVM-thin pool; they need a directory-backed
# datastore, same reasoning as the HAProxy VM's cloud-init file.
resource "proxmox_virtual_environment_file" "dev_vm_cloud_init" {
  content_type = "snippets"
  datastore_id = var.dev_vm_snippet_datastore
  node_name    = var.dev_vm_node

  source_raw {
    file_name = "${var.dev_vm_name}-cloud-init.yaml"
    data = templatefile("${path.module}/templates/dev-vm-cloud-init.yaml.tftpl", {
      hostname       = var.dev_vm_name
      admin_user     = var.dev_vm_user
      ssh_public_key = var.dev_vm_ssh_public_key
    })
  }
}

resource "proxmox_virtual_environment_vm" "dev_vm" {
  name      = var.dev_vm_name
  node_name = var.dev_vm_node
  vm_id     = var.dev_vm_id

  description = "Daily-driver dev VM: IDE (Remote-SSH), Nx/pnpm builds, Docker, Claude Code sessions"

  cpu {
    cores = var.dev_vm_cores
    type  = "host"
  }

  memory {
    dedicated = var.dev_vm_memory
  }

  # Left on the provider's SeaBIOS default, same as the HAProxy VM: an
  # Ubuntu cloud image boots fine on SeaBIOS and this VM needs no PCIe
  # passthrough, so there's no reason to take on an EFI disk to keep in sync.

  disk {
    datastore_id = var.dev_vm_storage_pool
    interface    = "scsi0"
    size         = var.dev_vm_disk_size
    iothread     = true
    discard      = "on"
    import_from  = proxmox_virtual_environment_download_file.dev_vm_cloud_image.id
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
    datastore_id = var.dev_vm_storage_pool

    # Static, never a DHCP lease — same rule as every other VM here, and this
    # one specifically needs a stable address for Remote-SSH host entries.
    ip_config {
      ipv4 {
        address = var.dev_vm_ip
        gateway = var.dev_vm_gateway
      }
    }

    user_data_file_id = proxmox_virtual_environment_file.dev_vm_cloud_init.id
  }

  # A daily-driver box should come back up unattended after pve1 restarts.
  on_boot = true

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0"]
  tags            = ["dev", "workstation"]
}

output "dev_vm_address" {
  description = "Provisioned dev VM identity, for the bootstrap and migration runbooks."
  value = {
    vmid = proxmox_virtual_environment_vm.dev_vm.vm_id
    node = proxmox_virtual_environment_vm.dev_vm.node_name
    ip   = split("/", var.dev_vm_ip)[0]
  }
}
