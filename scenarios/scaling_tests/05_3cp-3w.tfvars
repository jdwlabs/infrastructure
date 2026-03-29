# Test 05: Scale Workers Down - 3 CP + 3 Workers (non-sequential)
cluster_name = "test-core"

proxmox_endpoint         = "https://192.168.1.200:8006/api2/json"
proxmox_api_token_id     = "terraform@pve!cluster"
proxmox_api_token_secret = "9c85e97c-ec5e-43e1-b6ad-a0b46268a600"

# Go bootstrapper configuration
control_plane_endpoint = "cluster.jdwlabs.com"
haproxy_ip = "192.168.1.199"
haproxy_login_user = "root"
haproxy_stats_user = "admin"
haproxy_stats_password = "admin"
kubernetes_version = "v1.35.1"
talos_version = "v1.12.3"
installer_image = "factory.talos.dev/nocloud-installer/b553b4a25d76e938fd7a9aaa7f887c06ea4ef75275e64f4630e6f8f739cf07df:v1.12.3"

proxmox_node_ips = {
  pve1 = "192.168.1.200"
  pve2 = "192.168.1.201"
  pve3 = "192.168.1.202"
  pve4 = "192.168.1.203"
}

storage_pool = "local-lvm"
talos_iso    = "local:iso/nocloud-amd64.iso"

talos_control_configuration = [
  { node_name = "pve2", vm_name = "talos-cp-01", vmid = 200, cpu_cores = 4, memory = 4096, disk_size = 50 },
  { node_name = "pve3", vm_name = "talos-cp-02", vmid = 201, cpu_cores = 4, memory = 4096, disk_size = 50 },
  { node_name = "pve4", vm_name = "talos-cp-03", vmid = 202, cpu_cores = 4, memory = 4096, disk_size = 50 },
]

talos_worker_configuration = [
  { node_name = "pve1", vm_name = "talos-worker-01", vmid = 300, cpu_cores = 2, memory = 4096, disk_size = 40 },
  { node_name = "pve4", vm_name = "talos-worker-08", vmid = 307, cpu_cores = 2, memory = 4096, disk_size = 40 },
  { node_name = "pve4", vm_name = "talos-worker-10", vmid = 309, cpu_cores = 2, memory = 4096, disk_size = 40 },
]
