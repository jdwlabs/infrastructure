# GPU inference VM on pve5, OUTSIDE Talos, isolating vLLM from the fragile
# control plane. Holds the RTX 5090 via PCIe passthrough (whole device
# 0000:01:00 — VGA + audio functions share IOMMU group 13); LiteLLM reaches
# it over the LAN. Host prerequisites: IOMMU enabled (already on) and the GPU
# bound to vfio-pci (nouveau blacklisted) before the VM can start.

# content_type "import" + .qcow2 name: lets the VM disk use API-based
# import_from instead of the provider's node-SSH importdisk path, which
# cannot reach an ssh-agent from this workstation.
resource "proxmox_virtual_environment_download_file" "ubuntu_cloud_image" {
  content_type       = "import"
  datastore_id       = "local"
  node_name          = var.gpu_vm_node
  file_name          = "ubuntu-24.04-server-cloudimg-amd64.qcow2"
  url                = "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img"
  checksum           = "d0fe84bb5f80853425fa6be28e2c106f30104c3cfe8611933f2e65c9b63f0e30"
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

resource "proxmox_virtual_environment_file" "gpu_cloud_init" {
  content_type = "snippets"
  datastore_id = var.gpu_vm_snippet_datastore
  node_name    = var.gpu_vm_node

  source_raw {
    file_name = "${var.gpu_vm_name}-cloud-init.yaml"
    data = templatefile("${path.module}/templates/gpu-cloud-init.yaml.tftpl", {
      hostname       = var.gpu_vm_name
      admin_user     = var.gpu_vm_user
      ssh_public_key = var.gpu_vm_ssh_public_key
    })
  }
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

    # Kept alongside user_data_file_id rather than removed: the pinned
    # provider's read rebuilds this block from PVE's live ciuser/sshkeys on
    # every refresh, so dropping it from config plans an in-place
    # initialization update against that reconstructed state — and by
    # default the provider applies an initialization change by rebooting the
    # VM to apply it (a full QEMU power cycle, not a config-only write). It
    # is inert once the VM is actually rebuilt from this config
    # (`-replace`): the rendered snippet's `cicustom user=` runs cloud-init's
    # own user creation, the same mechanism the dev/devbox2/haproxy VMs rely
    # on, and this block never gets a chance to fight it.
    user_account {
      username = var.gpu_vm_user
      keys     = [var.gpu_vm_ssh_public_key]
    }

    user_data_file_id = proxmox_virtual_environment_file.gpu_cloud_init.id
  }

  # Workstation host: the VM must come back up unattended after pve5 restarts.
  on_boot = true

  # vmUpdate sets rebootRequired on any initialization/cpu/memory/hostpci/
  # vga/machine change; the provider's default is to reboot the VM
  # immediately to apply it. With this set, that class of change instead
  # writes PVE's pending config and warns that a manual reboot is needed —
  # it does not fail the apply, so "Apply complete!" plus that warning must
  # still be read as a pending change, not as nothing changed. It takes
  # effect at the VM's next reboot (e.g. a pve5 restart, since on_boot =
  # true). This card has already wedged after a reboot; the point is that
  # one now happens only when something deliberately reboots the VM, never
  # as a side effect of an unrelated apply.
  reboot_after_update = false

  stop_on_destroy = true
  scsi_hardware   = "virtio-scsi-single"
  boot_order      = ["scsi0"]
  tags            = ["ai-sre", "gpu", "vllm"]

  # user_data_file_id and the file resource behind it are both ForceNew in
  # the pinned provider (see the HAProxy VM for the mechanism, confirmed
  # against the provider source there). This is a rebuild-only path:
  # ignore_changes keeps user_data_file_id — and therefore cicustom — unset
  # on this already-running guest, so setting it in config here does not
  # reach the live VM at all; the snippet only takes effect the next time
  # this VM is actually replaced (`-replace`). Without this guard any
  # template edit replaces the VM, and plan/apply would do so silently; CI
  # only runs validate.
  lifecycle {
    ignore_changes = [initialization[0].user_data_file_id]
  }
}
