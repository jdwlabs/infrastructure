terraform {
  required_version = ">= 1.5.0"
  required_providers {
    proxmox = {
      source  = "bpg/proxmox" # Use newer provider
      version = ">= 0.53.1"
    }
  }
}

provider "proxmox" {
  endpoint  = var.proxmox_endpoint
  api_token = "${var.proxmox_api_token_id}=${var.proxmox_api_token_secret}"
  insecure  = true

  # Cloud-image disk imports (qm importdisk) run over node SSH, not the API;
  # the provider only reads keys from a running ssh-agent.
  ssh {
    agent    = true
    username = "root"
  }
}
