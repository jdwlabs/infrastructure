# GPU inference VM on pve5, OUTSIDE Talos, isolating vLLM from the fragile
# control plane. Holds the RTX 5090 via PCIe passthrough (whole device
# 0000:01:00 — VGA + audio functions share IOMMU group 13); LiteLLM reaches
# it over the LAN. Host prerequisites: IOMMU enabled (already on) and the GPU
# bound to vfio-pci (nouveau blacklisted) before the VM can start.

# content_type "import" + .qcow2 name: lets the VM disk use API-based
# import_from instead of the provider's node-SSH importdisk path, which
# cannot reach an ssh-agent from this workstation.
resource "proxmox_virtual_environment_download_file" "ubuntu_cloud_image" {
  content_type = "import"
  datastore_id = "local"
  node_name    = var.gpu_vm_node
  file_name    = "ubuntu-24.04-server-cloudimg-amd64.qcow2"
  url          = "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img"
}

resource "proxmox_virtual_environment_vm" "gpu_inference" {
  name      = var.gpu_vm_name
  node_name = var.gpu_vm_node
  vm_id     = var.gpu_vm_id

  description = "vLLM inference host: RTX 5090 passthrough for the AI-SRE local model tier"

  # q35 + OVMF are required for PCIe (not legacy PCI) passthrough.
  machine = "q35"
  bios    = "ovmf"
  efi_disk {
    datastore_id = var.storage_pool
    type         = "4m"
  }

  cpu {
    cores = var.gpu_vm_cores
    type  = "host"
  }

  memory {
    dedicated = var.gpu_vm_memory
  }

  # Cluster PCI resource mapping, not a raw PCI id: Proxmox only lets root
  # set hostpci for non-mapped devices, and terraform runs as an API token
  # (needs Mapping.Use on /mapping/pci/<name>, granted via PVEMappingUser).
  hostpci {
    device  = "hostpci0"
    mapping = var.gpu_pci_mapping
    pcie    = true
  }

  disk {
    datastore_id = var.storage_pool
    interface    = "scsi0"
    size         = var.gpu_vm_disk_size
    iothread     = true
    discard      = "on"
    import_from  = proxmox_virtual_environment_download_file.ubuntu_cloud_image.id
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
    ip_config {
      ipv4 {
        address = var.gpu_vm_ip
        gateway = var.gpu_vm_gateway
      }
    }
    user_account {
      username = var.gpu_vm_user
      keys     = [var.gpu_vm_ssh_public_key]
    }
  }

  # Workstation host: the VM must come back up unattended after pve5 restarts.
  on_boot = true

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0"]
  tags            = ["ai-sre", "gpu", "vllm"]
}
