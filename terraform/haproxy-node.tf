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
