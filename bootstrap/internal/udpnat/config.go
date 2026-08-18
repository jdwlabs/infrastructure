// Package udpnat renders nftables DNAT rules that publish Kubernetes UDP
// NodePort services on the HAProxy VM.
//
// HAProxy itself is TCP-only, so UDP workloads (Minecraft Bedrock being the
// first) cannot ride the frontends in the haproxy package. They still want the
// same shape: one address at the edge, one router rule, and everything after
// that driven from cluster state rather than hand-edited.
//
// The rules live in their own nftables table and never flush the ruleset. The
// VM also runs Tailscale, whose rules sit in table ip filter and are managed at
// runtime; /etc/nftables.conf ships with "flush ruleset" at the top, which is
// why nftables.service stays disabled on that host and these rules are applied
// by a dedicated unit instead.
package udpnat

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"text/template"
)

// Forward is a single external UDP port published to a NodePort.
type Forward struct {
	Name         string // service identity, for the rule comment only
	ExternalPort int    // port the edge listens on, what players connect to
	NodePort     int    // NodePort the traffic is rewritten to
}

// Config holds everything needed to render the nftables ruleset.
type Config struct {
	TargetIP net.IP    // node receiving the DNATed traffic
	Forwards []Forward // sorted by ExternalPort before rendering
}

const nftTemplate = `#!/usr/sbin/nft -f
# Managed file. Generated from cluster state - do not edit by hand.
#
# Publishes Kubernetes UDP NodePort services at this host's address so a single
# router rule can serve every one of them.
#
# Scoped to its own table and deliberately without "flush ruleset": this host
# also runs Tailscale, whose rules live in table ip filter. Flushing would drop
# them, which is why nftables.service is disabled here and a dedicated unit
# applies this file.
table ip minecraft
delete table ip minecraft
table ip minecraft {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
{{- range .Forwards }}
    udp dport {{ .ExternalPort }} dnat to {{ $.TargetIP }}:{{ .NodePort }} comment "{{ .Name }}"
{{- end }}
  }

  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
{{- range .Forwards }}
    ip daddr {{ $.TargetIP }} udp dport {{ .NodePort }} masquerade comment "{{ .Name }}"
{{- end }}
  }
}
`

// Generate renders the nftables ruleset. Forwards are sorted by external port
// so repeated runs over unordered input produce a byte-identical file, which is
// what lets a diff against the live host mean something.
func (c *Config) Generate() (string, error) {
	if c.TargetIP == nil {
		return "", fmt.Errorf("udpnat: TargetIP is required")
	}

	seen := make(map[int]string, len(c.Forwards))
	for _, f := range c.Forwards {
		if f.ExternalPort <= 0 || f.ExternalPort > 65535 {
			return "", fmt.Errorf("udpnat: %s has invalid external port %d", f.Name, f.ExternalPort)
		}
		if f.NodePort <= 0 || f.NodePort > 65535 {
			return "", fmt.Errorf("udpnat: %s has invalid node port %d", f.Name, f.NodePort)
		}
		// Two services claiming one external port silently drops one of them,
		// and the survivor depends on rule order. Refuse instead.
		if prev, dup := seen[f.ExternalPort]; dup {
			return "", fmt.Errorf("udpnat: external port %d claimed by both %s and %s",
				f.ExternalPort, prev, f.Name)
		}
		seen[f.ExternalPort] = f.Name
	}

	sorted := make([]Forward, len(c.Forwards))
	copy(sorted, c.Forwards)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ExternalPort < sorted[j].ExternalPort })

	tmpl, err := template.New("nft").Parse(nftTemplate)
	if err != nil {
		return "", fmt.Errorf("udpnat: parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, &Config{TargetIP: c.TargetIP, Forwards: sorted}); err != nil {
		return "", fmt.Errorf("udpnat: rendering template: %w", err)
	}
	return buf.String(), nil
}
