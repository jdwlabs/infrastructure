terraform {
  # use_lockfile (native S3 state locking) requires Terraform >= 1.10
  required_version = ">= 1.15.8"
  required_providers {
    proxmox = {
      source = "bpg/proxmox"
      # Pessimistic patch-level pin: the committed .terraform.lock.hcl holds the
      # exact version; this window allows deliberate `init -upgrade` patch bumps
      # without editing HCL, while blocking surprise minor-version jumps.
      version = "~> 0.111.1"
    }
  }

  # Remote state on MinIO (TrueNAS, S3-compatible). No credentials here:
  # the backend reads the standard AWS credential chain (AWS_ACCESS_KEY_ID/
  # AWS_SECRET_ACCESS_KEY env vars, or the default profile in
  # ~/.aws/credentials). Source of truth: terraform/backend-credentials.enc.yaml
  # (SOPS vault) — see docs/secrets.md. The skip_* flags disable AWS-account
  # validations that a self-hosted S3 endpoint cannot answer; region is a
  # required-but-meaningless placeholder for MinIO.
  backend "s3" {
    bucket = "terraform-state"
    key    = "infrastructure/terraform.tfstate"
    region = "us-east-1"
    endpoints = {
      s3 = "https://192.168.1.205:9000"
    }
    # MinIO serves a cert issued by the internal CA committed alongside this
    # file (private halves vaulted in backend-tls.enc.yaml). The relative path
    # resolves against the process working directory — talops and manual runs
    # both execute terraform from this directory. AWS_CA_BUNDLE overrides it.
    custom_ca_bundle            = "minio-ca.crt"
    use_path_style              = true
    use_lockfile                = true
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
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
