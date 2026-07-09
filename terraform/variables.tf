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
