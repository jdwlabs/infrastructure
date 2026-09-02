# Proxmox Authentication
variable "proxmox_endpoint" {
  type        = string
  description = "Proxmox API endpoint (e.g., https://192.168.1.100:8006/api2/json)"
}

variable "proxmox_api_token_id" {
  type        = string
  description = "API Token ID: terraform@pve!token-name"
  sensitive   = true
}

variable "proxmox_api_token_secret" {
  type        = string
  description = "API Token Secret"
  sensitive   = true
}

# Infrastructure Settings
variable "storage_pool" {
  type    = string
  default = "local-lvm"
}

variable "talos_iso" {
  type    = string
  default = "local:iso/nocloud-amd64.iso"
}

# Go Bootstrapper Configuration
# Read by the Go bootstrap tool, not used by Terraform.
# Declared here to supress "undeclared variable" warnings.
variable "cluster_name" {
  type    = string
  default = null
}

variable "control_plane_endpoint" {
  type    = string
  default = null
}

variable "haproxy_ip" {
  type    = string
  default = null
}

variable "haproxy_login_user" {
  type    = string
  default = null
}

variable "haproxy_stats_user" {
  type    = string
  default = null
}

variable "haproxy_stats_password" {
  type      = string
  sensitive = true
  default   = null
}

variable "admin_allowed_cidrs" {
  description = "Source CIDRs allowed to reach the HAProxy k8s-apiserver (6443) and talos-apiserver (50000) frontends. Empty list means unrestricted (current behavior)."
  type        = list(string)
  default     = []
}

variable "kubernetes_version" {
  type    = string
  default = null
}

variable "talos_version" {
  type    = string
  default = null
}

variable "installer_image" {
  type    = string
  default = null
}

variable "proxmox_node_ips" {
  type    = map(string)
  default = null
}

variable "ingress_http_nodeport" {
  type    = number
  default = null
}

variable "ingress_tls_nodeport" {
  type    = number
  default = null
}

# CONTROL PLANE CONFIGURATION
# This is a LIST of objects - add more objects to scale up
variable "talos_control_configuration" {
  description = "List of control plane node configs"
  type = list(object({
    node_name = string
    vm_name   = string
    vmid      = number
    cpu_cores = number
    memory    = number
    disk_size = number
  }))
}

# WORKER CONFIGURATION
# This is a LIST of objects - add more objects to scale up
variable "talos_worker_configuration" {
  description = "List of worker node configs"
  type = list(object({
    node_name = string
    vm_name   = string
    vmid      = number
    cpu_cores = number
    memory    = number
    disk_size = number
    # Dedicated Longhorn data disk (scsi1), GiB. Optional so nodes can be
    # migrated one at a time; omit to keep a node on its root disk. The size
    # doubles as the Talos disk-selector discriminator in the per-node machine
    # patch (clusters/core/patches/node-<vmid>.yaml) — the two must agree or
    # the selector matches nothing.
    data_disk_size = optional(number)
  }))
}

# GPU INFERENCE VM (pve5 / RTX 5090 — AI-SRE local model tier)
variable "gpu_vm_name" {
  description = "Name of the GPU inference VM on pve5."
  type        = string
  default     = "vllm-inference"
}

variable "gpu_vm_node" {
  description = "Proxmox node hosting the GPU."
  type        = string
  default     = "pve5"
}

variable "gpu_vm_id" {
  description = "Proxmox VMID for the GPU inference VM."
  type        = number
  default     = 500
}

variable "gpu_pci_mapping" {
  description = "Cluster PCI resource mapping name for the RTX 5090 (pvesh /cluster/mapping/pci). Whole-device mapping passes VGA + audio together."
  type        = string
  default     = "gpu-rtx5090"
}

variable "gpu_vm_cores" {
  description = "vCPU cores for the GPU VM."
  type        = number
  default     = 8
}

variable "gpu_vm_memory" {
  description = "Dedicated memory (MiB) for the GPU VM."
  type        = number
  default     = 32768
}

variable "gpu_vm_disk_size" {
  description = "Root disk size (GiB). Model weights are large; fp8 ~35B needs ~40GiB plus headroom."
  type        = number
  default     = 200
}

variable "gpu_vm_ip" {
  description = "Static LAN IP for the GPU VM (CIDR)."
  type        = string
  default     = "192.168.1.50/24"
}

variable "gpu_vm_gateway" {
  description = "Default gateway for the GPU VM. The LAN gateway is .254, not .1 (verified: pve5's own default route)."
  type        = string
  default     = "192.168.1.254"
}

variable "gpu_vm_user" {
  description = "Cloud-init admin user on the GPU VM."
  type        = string
  default     = "vllm"
}

variable "gpu_vm_ssh_public_key" {
  description = "SSH public key granted to the cloud-init user."
  type        = string
}

# HAPROXY LOAD BALANCER VM(S)
# A LIST of objects, like the control-plane and worker variables. Empty by
# default so a checkout without haproxy_vms in tfvars provisions nothing —
# the load balancer currently fronting the cluster was built by hand and is
# not represented in this state. A second element is all a keepalived VIP
# pair needs later, which is why this is a list and not a flat set of scalars.
variable "haproxy_vms" {
  description = "List of HAProxy load-balancer VM configs. Empty means provision nothing."
  type = list(object({
    node_name = string
    vm_name   = string
    vmid      = number
    cpu_cores = number
    memory    = number
    disk_size = number
    # Static address in CIDR form. Must not be a DHCP lease: this address is
    # what DNS, every talosconfig endpoint, and kubeconfig resolve to.
    ip = string
  }))
  default = []

  validation {
    condition = alltrue([
      for vm in var.haproxy_vms : can(cidrnetmask(vm.ip))
    ])
    error_message = "Each haproxy_vms entry needs an explicit static address in CIDR form (e.g. 192.168.1.199/24); DHCP is not acceptable for the API endpoint."
  }

  validation {
    condition     = length(distinct([for vm in var.haproxy_vms : vm.vmid])) == length(var.haproxy_vms)
    error_message = "haproxy_vms entries must have distinct vmid values."
  }

  validation {
    condition     = length(distinct([for vm in var.haproxy_vms : vm.ip])) == length(var.haproxy_vms)
    error_message = "haproxy_vms entries must have distinct ip values."
  }
}

variable "haproxy_gateway" {
  description = "Default gateway for the HAProxy VMs. The LAN gateway is .254, not .1."
  type        = string
  default     = "192.168.1.254"
}

variable "haproxy_admin_user" {
  description = "Cloud-init admin user created on the HAProxy VMs. The config-push path escalates through sudo, so this need not be root."
  type        = string
  default     = "haproxy-admin"
}

variable "haproxy_ssh_public_key" {
  description = "SSH public key granted to the HAProxy cloud-init admin user."
  type        = string
  default     = null
}

variable "haproxy_cloud_image_url" {
  description = "Ubuntu cloud image used as the HAProxy VM root disk."
  type        = string
  default     = "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img"
}

variable "haproxy_cloud_image_checksum" {
  description = "SHA256 of haproxy_cloud_image_url's current contents. Update together — a URL change with no matching checksum change fails the download intentionally."
  type        = string
  default     = "0533b0655c32e68b31d792ecd6ccfca95abdbc536c4446874fe0513bd4140ffe"
}

variable "haproxy_image_datastore" {
  description = "Datastore holding the imported cloud image. Needs the 'import' content type."
  type        = string
  default     = "local"
}

variable "haproxy_snippet_datastore" {
  description = "Datastore holding the cloud-init user-data snippet. Needs the 'snippets' content type, which an LVM-thin pool cannot provide."
  type        = string
  default     = "local"
}

# DEV VM (pve5 — daily-driver dev box, see docs/dev-vm-provisioning.md)
# Flat vars, not a list: single VM, no HA requirement (unlike haproxy_vms).
variable "dev_vm_node" {
  description = "Proxmox node hosting the dev VM. Not pve1: Phase 0 capacity check (2026-08-09) found pve1 has only 28.2GiB total RAM with ~21GiB already allocated to running guests — an 8c/32GB VM cannot fit. pve5 is the only non-control-plane host with room (123.5GiB total, ~42GiB free at check time)."
  type        = string
  default     = "pve5"
}

variable "dev_vm_name" {
  description = "Hostname of the dev VM."
  type        = string
  default     = "devbox"
}

variable "dev_vm_id" {
  description = "Proxmox VMID for the dev VM. Next free after haproxy's 110."
  type        = number
  default     = 111
}

variable "dev_vm_cores" {
  description = "vCPU cores for the dev VM. Sized for IDE + Nx/pnpm builds + Docker + multiple concurrent agent sessions, not light editing."
  type        = number
  default     = 8
}

variable "dev_vm_memory" {
  description = "Dedicated memory (MiB) for the dev VM."
  type        = number
  default     = 32768
}

variable "dev_vm_disk_size" {
  description = "Root disk size (GiB), on the NFS-backed datastore so the VM can live-migrate."
  type        = number
  default     = 300
}

variable "dev_vm_ip" {
  description = "Static LAN IP for the dev VM (CIDR). Below the DHCP pool floor (.64), adjacent to haproxy's .55 and the GPU VM's .50."
  type        = string
  default     = "192.168.1.56/24"
}

variable "dev_vm_gateway" {
  description = "Default gateway for the dev VM. The LAN gateway is .254, not .1."
  type        = string
  default     = "192.168.1.254"
}

variable "dev_vm_user" {
  description = "Cloud-init admin user on the dev VM."
  type        = string
  default     = "dev-admin"
}

variable "dev_vm_ssh_public_key" {
  description = "SSH public key granted to the dev VM cloud-init admin user."
  type        = string
  default     = null
}

variable "dev_vm_storage_pool" {
  description = "Datastore for the dev VM's root disk. `truenas-vmdisks` (NFS, cluster-wide, backed by TrueNAS storage/proxmox) already exists — verified live via the Proxmox API on 2026-08-09, active on every node, currently empty. Not local/local-lvm — that's what makes online migration between Proxmox hosts possible."
  type        = string
  default     = "truenas-vmdisks"
}

variable "dev_vm_cloud_image_url" {
  description = "Ubuntu cloud image used as the dev VM root disk."
  type        = string
  default     = "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img"
}

variable "dev_vm_cloud_image_checksum" {
  description = "SHA256 of dev_vm_cloud_image_url's current contents. Update together — a URL change with no matching checksum change fails the download intentionally."
  type        = string
  default     = "0533b0655c32e68b31d792ecd6ccfca95abdbc536c4446874fe0513bd4140ffe"
}

variable "dev_vm_image_datastore" {
  description = "Datastore holding the imported cloud image. Needs the 'import' content type."
  type        = string
  default     = "local"
}

variable "dev_vm_snippet_datastore" {
  description = "Datastore holding the cloud-init user-data snippet. Needs the 'snippets' content type, which an LVM-thin pool cannot provide."
  type        = string
  default     = "local"
}

variable "devbox2_node" {
  description = "Proxmox node hosting the lifeboat VM. Must not be devbox's host (pve5) — a lifeboat that dies with the machine it exists to survive is not a lifeboat. pve1 is the only non-control-plane host with unallocated memory: 23G of its 28.2G is committed to talos-worker-01 and haproxy-1."
  type        = string
  default     = "pve1"
}

variable "devbox2_name" {
  description = "Hostname of the lifeboat VM."
  type        = string
  default     = "devbox2"
}

variable "devbox2_id" {
  description = "Proxmox VMID for the lifeboat VM. Next free after devbox's 111."
  type        = number
  default     = 112
}

variable "devbox2_cores" {
  description = "vCPU cores for the lifeboat VM. pve1 has 4 unallocated threads of 16 and runs at single-digit utilisation; CPU is not the scarce resource on this host."
  type        = number
  default     = 2
}

variable "devbox2_memory" {
  description = "Dedicated memory (MiB) for the lifeboat VM. Deliberately small: this takes pve1 to 25G committed of 28.2G, leaving 3.2G for the hypervisor. Enough for a shell, the admin toolchain and one agent session — not a second daily driver."
  type        = number
  default     = 2048
}

variable "devbox2_disk_size" {
  description = "Root disk size (GiB) for the lifeboat VM."
  type        = number
  default     = 32
}

variable "devbox2_ip" {
  description = "Static LAN IP for the lifeboat VM (CIDR). Below the DHCP pool floor (.64), adjacent to devbox's .56."
  type        = string
  default     = "192.168.1.57/24"
}

variable "devbox2_gateway" {
  description = "Default gateway for the lifeboat VM. The LAN gateway is .254, not .1."
  type        = string
  default     = "192.168.1.254"
}

variable "devbox2_user" {
  description = "Cloud-init admin user on the lifeboat VM. Matches devbox's so existing SSH config and habits carry over unchanged."
  type        = string
  default     = "dev-admin"
}

variable "devbox2_storage_pool" {
  description = "Datastore for the lifeboat VM's root disk. NFS rather than pve1's local-lvm: keeps the VM migratable and leaves pve1's local storage for talos-worker-01."
  type        = string
  default     = "truenas-vmdisks"
}

variable "devbox2_image_datastore" {
  description = "Datastore holding the imported cloud image. Needs the 'import' content type, and must live on devbox2_node."
  type        = string
  default     = "local"
}

variable "devbox2_snippet_datastore" {
  description = "Datastore holding the cloud-init user-data snippet. Needs the 'snippets' content type, which an LVM-thin pool cannot provide. Host-local, same as haproxy-1's on this node — see docs/devbox2-provisioning.md on why that is a drift risk rather than a migration blocker."
  type        = string
  default     = "local"
}
