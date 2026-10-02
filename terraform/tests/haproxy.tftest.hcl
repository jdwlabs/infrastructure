# Offline: the provider is mocked, so these plans touch neither Proxmox nor the
# state backend. Every variable the assertions depend on is set here, because
# `terraform test` also loads a terraform.tfvars sitting next to the config and
# a hydrated vault must not be able to change what these runs prove.

mock_provider "proxmox" {}

variables {
  proxmox_endpoint            = "https://192.0.2.1:8006/api2/json"
  proxmox_api_token_id        = "fixture@pve!fixture"
  proxmox_api_token_secret    = "fixture"
  talos_control_configuration = []
  talos_worker_configuration  = []
  gpu_vm_ssh_public_key       = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKeyOnlyNotARealOne test@fixture"
  dev_vm_ssh_public_key       = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKeyOnlyNotARealOne test@fixture"
  haproxy_ssh_public_key      = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKeyOnlyNotARealOne test@fixture"
  haproxy_admin_user          = "haproxy-admin"
  haproxy_ip                  = "192.168.1.199"
  haproxy_gateway             = "192.168.1.254"
  haproxy_vrrp_router_id      = 51
  haproxy_vrrp_interface      = "eth0"
  haproxy_vrrp_auth_pass      = null
  haproxy_vms                 = []
}

run "standalone_snippet_is_unchanged" {
  command = plan

  variables {
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.199/24" },
    ]
  }

  # The digest is of the snippet as it rendered before the VRRP group existed,
  # for these fixture inputs. It moves only when the bytes a standalone load
  # balancer boots with move — and that is a change to schedule, because
  # replacing the snippet re-runs cloud-init on the live VM at its next boot
  # (terraform/README.md, "The part the guards do not fix"). Update it
  # deliberately, alongside the reboot the change implies.
  assert {
    condition     = sha256(proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-1"].source_raw[0].data) == "ab513386e92af9e70d123567d5633862dba105da84d82d14fa2900775fe7142e"
    error_message = "The standalone cloud-init snippet no longer renders the bytes it did. Applying this replaces the snippet and re-runs cloud-init on the live load balancer at its next boot."
  }

  assert {
    condition     = !strcontains(proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-1"].source_raw[0].data, "/etc/keepalived/")
    error_message = "A standalone load balancer must not be given keepalived: its own address is the endpoint."
  }
}

run "group_renders_keepalived_per_instance" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "Fixture8"
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.11/24", vrrp_priority = 150 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24", vrrp_priority = 100 },
    ]
  }

  assert {
    condition = alltrue([
      for line in [
        "  - keepalived",
        "          priority 150",
        "          unicast_src_ip 192.168.1.11",
        "              192.168.1.12\n          }",
        "              192.168.1.199/24 dev eth0",
        "          virtual_router_id 51",
        "              auth_pass Fixture8",
        "grep -qF \"inet 192.168.1.11/\"",
      ] : strcontains(nonsensitive(proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-1"].source_raw[0].data), line)
    ])
    error_message = "haproxy-1's snippet must carry its own priority and source address, its peer, and the shared virtual address."
  }

  assert {
    condition = alltrue([
      for line in [
        "          priority 100",
        "          unicast_src_ip 192.168.1.12",
        "              192.168.1.11\n          }",
        "grep -qF \"inet 192.168.1.12/\"",
      ] : strcontains(nonsensitive(proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-2"].source_raw[0].data), line)
    ])
    error_message = "haproxy-2's snippet must carry its own priority and source address, and haproxy-1 as its peer."
  }

  # Neither instance is configured to preempt, so the election is the only
  # place priority is read. A MASTER initial state would bypass it.
  assert {
    condition = alltrue([
      for name in ["haproxy-1", "haproxy-2"] :
      strcontains(nonsensitive(proxmox_virtual_environment_file.haproxy_cloud_init[name].source_raw[0].data), "          state BACKUP\n          nopreempt\n")
    ])
    error_message = "Every instance must start as BACKUP with nopreempt."
  }

  assert {
    condition     = can(yamldecode(nonsensitive(proxmox_virtual_environment_file.haproxy_cloud_init["haproxy-2"].source_raw[0].data)))
    error_message = "The rendered snippet must still be valid YAML."
  }
}

run "group_rejects_an_instance_on_the_virtual_address" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "Fixture8"
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.199/24", vrrp_priority = 150 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24", vrrp_priority = 100 },
    ]
  }

  expect_failures = [var.haproxy_vms]
}

run "group_rejects_a_missing_priority" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "Fixture8"
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.11/24", vrrp_priority = 150 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24" },
    ]
  }

  expect_failures = [var.haproxy_vms]
}

# Terraform's number type admits fractions; keepalived's priority does not,
# and the instance would boot with a config keepalived refuses to load.
run "group_rejects_a_fractional_priority" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "Fixture8"
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.11/24", vrrp_priority = 150.5 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24", vrrp_priority = 100 },
    ]
  }

  expect_failures = [var.haproxy_vms]
}

run "router_id_must_be_a_whole_number" {
  command = plan

  variables {
    haproxy_vrrp_router_id = 51.5
  }

  expect_failures = [var.haproxy_vrrp_router_id]
}

run "group_rejects_equal_priorities" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "Fixture8"
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.11/24", vrrp_priority = 100 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24", vrrp_priority = 100 },
    ]
  }

  expect_failures = [var.haproxy_vms]
}

run "group_rejects_a_missing_secret" {
  command = plan

  variables {
    haproxy_vms = [
      { node_name = "pve1", vm_name = "haproxy-1", vmid = 110, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.11/24", vrrp_priority = 150 },
      { node_name = "pve5", vm_name = "haproxy-2", vmid = 113, cpu_cores = 2, memory = 1024, disk_size = 10, ip = "192.168.1.12/24", vrrp_priority = 100 },
    ]
  }

  expect_failures = [var.haproxy_vms]
}

run "secret_must_be_eight_plain_characters" {
  command = plan

  variables {
    haproxy_vrrp_auth_pass = "too-long-and-punctuated"
  }

  expect_failures = [var.haproxy_vrrp_auth_pass]
}
