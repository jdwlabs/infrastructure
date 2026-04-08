package haproxy

import (
	"bytes"
	"fmt"
	"net"
	"text/template"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

// Backend represents a control plane backend server in HAProxy
type Backend struct {
	VMID types.VMID
	IP   net.IP
}

// Config holds all data needed to generate an HAProxy configuration
type Config struct {
	HAProxyIP     net.IP
	StatsUser     string
	StatsPassword string
	ControlPlanes []Backend
	IngressNodes  []Backend // All nodes running ingress-nginx (CPs + workers)
}

const haproxyTemplate = `# ======= GLOBAL =======
global
    log /dev/log local0 info
    log /dev/log local1 notice
    chroot /var/lib/haproxy
    stats socket /run/haproxy/admin.sock mode 660 level admin
    stats timeout 30s
    user haproxy
    group haproxy
    maxconn 32000
    ulimit-n 65535
    nbthread 4
    cpu-map auto:1/1-4 0-3
    tune.ssl.default-dh-param 2048
    daemon

# ======= DEFAULTS =======
defaults
    mode tcp
    log global
    option tcplog
    option tcp-check
    option dontlognull
    option tcp-smart-connect
    option redispatch
    retries 3
    timeout connect 5s
    timeout client 30s
    timeout server 30s
    timeout check 5s
    maxconn 32000

# ======= STATS =======
listen stats
    mode http
    bind {{ .HAProxyIP }}:9000
    stats enable
    stats uri /
    stats refresh 5s
    stats show-legends
    stats admin if TRUE
{{- if and .StatsUser .StatsPassword }}
    stats auth {{ .StatsUser }}:{{ .StatsPassword }}
{{- end }}

# ======= KUBERNETES API =======
frontend k8s-apiserver
    mode tcp
    bind {{ .HAProxyIP }}:6443
    option tcplog
    tcp-request inspect-delay 5s
    tcp-request content accept if { req_ssl_hello_type 1 }
    default_backend k8s-controlplane

backend k8s-controlplane
    mode tcp
    balance leastconn
    option tcp-check
    tcp-check connect port 6443
    default-server inter 5s fall 3 rise 2
{{- range .ControlPlanes }}
    server talos-cp-{{ .VMID }} {{ .IP }}:6443 check
{{- end }}

# ======= TALOS API =======
frontend talos-apiserver
    mode tcp
    bind {{ .HAProxyIP }}:50000
    option tcplog
    tcp-request inspect-delay 5s
    tcp-request content accept if { req_ssl_hello_type 1 }
    default_backend talos-controlplane

backend talos-controlplane
    mode tcp
    balance leastconn
    option tcp-check
    tcp-check connect port 50000
    default-server inter 5s fall 3 rise 2
    timeout connect 10s
    timeout server 60s
{{- range .ControlPlanes }}
    server talos-cp-{{ .VMID }} {{ .IP }}:50000 check
{{- end }}
{{- if .IngressNodes }}

# ======= HTTP INGRESS =======
frontend http-ingress
    mode tcp
    bind {{ .HAProxyIP }}:80
    option tcplog
    default_backend ingress-http

backend ingress-http
    mode tcp
    balance roundrobin
    option tcp-check
    tcp-check connect port 30080
    default-server inter 5s fall 3 rise 2
{{- range .IngressNodes }}
    server ingress-{{ .VMID }} {{ .IP }}:30080 check send-proxy
{{- end }}

# ======= HTTPS INGRESS =======
frontend https-ingress
    mode tcp
    bind {{ .HAProxyIP }}:443
    option tcplog
    tcp-request inspect-delay 5s
    tcp-request content accept if { req_ssl_hello_type 1 }
    default_backend ingress-https

backend ingress-https
    mode tcp
    balance roundrobin
    option tcp-check
    tcp-check connect port 30443 ssl verify none
    default-server inter 5s fall 3 rise 2
{{- range .IngressNodes }}
    server ingress-{{ .VMID }} {{ .IP }}:30443 check send-proxy
{{- end }}
{{- end }}
`

// Generate renders the HAProxy configuration from the template
func (c *Config) Generate() (string, error) {
	if c.HAProxyIP == nil {
		return "", fmt.Errorf("HAProxy IP is required")
	}
	if len(c.ControlPlanes) == 0 {
		return "", fmt.Errorf("at least one control plane backend is required")
	}

	tmpl, err := template.New("haproxy").Parse(haproxyTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, c); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return buf.String(), nil
}

// ConfigFromClusterState builds an HAProxy Config from the current cluster state
func ConfigFromClusterState(cfg *types.Config, state *types.ClusterState) *Config {
	haConfig := &Config{
		HAProxyIP:     cfg.HAProxyIP,
		StatsUser:     cfg.HAProxyStatsUser,
		StatsPassword: cfg.HAProxyStatsPassword,
	}

	for _, cp := range state.ControlPlanes {
		if cp.IP == nil {
			continue
		}
		haConfig.ControlPlanes = append(haConfig.ControlPlanes, Backend{
			VMID: cp.VMID,
			IP:   cp.IP,
		})
		haConfig.IngressNodes = append(haConfig.IngressNodes, Backend{
			VMID: cp.VMID,
			IP:   cp.IP,
		})
	}

	for _, w := range state.Workers {
		if w.IP == nil {
			continue
		}
		haConfig.IngressNodes = append(haConfig.IngressNodes, Backend{
			VMID: w.VMID,
			IP:   w.IP,
		})
	}

	return haConfig
}
