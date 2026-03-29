resource "proxmox_virtual_environment_vm" "worker" {
  # Creates one VM per item in the list
  for_each = { for idx, cfg in var.talos_worker_configuration : cfg.vm_name => cfg }

  name      = each.value.vm_name
  node_name = each.value.node_name
  vm_id     = each.value.vmid

  description = "Talos Worker: ${each.value.vm_name}"

  cpu {
    cores = each.value.cpu_cores
    type  = "host"
  }

  memory {
    dedicated = each.value.memory
  }

  bios = "ovmf"
  efi_disk {
    datastore_id = var.storage_pool
    type         = "4m"
  }

  cdrom {
    file_id = var.talos_iso
  }

  disk {
    datastore_id = var.storage_pool
    interface    = "scsi0"
    size         = each.value.disk_size
    iothread     = true
    discard      = "on"
  }

  network_device {
    bridge = "vmbr0"
    model  = "virtio"
  }

  agent {
    enabled = true
    trim    = true
  }

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0", "ide3"]
  tags            = ["kubernetes", "worker", "talos"]
}
