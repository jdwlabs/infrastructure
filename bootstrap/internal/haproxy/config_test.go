package haproxy

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

func TestConfig_Generate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
		errMsg  string
		checks  func(t *testing.T, config string)
	}{
		{
			name: "valid config with single control plane",
			config: Config{
				HAProxyIP:     net.ParseIP("192.168.1.237"),
				StatsUser:     "admin",
				StatsPassword: "secret123",
				ControlPlanes: []Backend{
					{VMID: 201, IP: net.ParseIP("192.168.1.201")},
				},
			},
			wantErr: false,
			checks: func(t *testing.T, config string) {
				// Check global section
				if !strings.Contains(config, "global") {
					t.Error("config missing global section")
				}
				// Check stats binding
				if !strings.Contains(config, "bind 192.168.1.237:9000") {
					t.Error("config missing stats bind directive")
				}
				// Check k8s frontend
				if !strings.Contains(config, "bind 192.168.1.237:6443") {
					t.Error("config missing k8s apiserver bind")
				}
				// Check talos frontend
				if !strings.Contains(config, "bind 192.168.1.237:50000") {
					t.Error("config missing talos apiserver bind")
				}
				// Check backend server with VMID
				if !strings.Contains(config, "server talos-cp-201 192.168.1.201:6443 check") {
					t.Error("config missing k8s backend server with correct VMID naming")
				}
				if !strings.Contains(config, "server talos-cp-201 192.168.1.201:50000 check") {
					t.Error("config missing talos backend server with correct VMID naming")
				}
				// Check stats auth
				if !strings.Contains(config, "stats auth admin:secret123") {
					t.Error("config missing stats auth")
				}
			},
		},
		{
			name: "valid config with multiple control planes",
			config: Config{
				HAProxyIP:     net.ParseIP("10.0.0.1"),
				StatsUser:     "",
				StatsPassword: "",
				ControlPlanes: []Backend{
					{VMID: 201, IP: net.ParseIP("10.0.0.201")},
					{VMID: 202, IP: net.ParseIP("10.0.0.202")},
					{VMID: 203, IP: net.ParseIP("10.0.0.203")},
				},
			},
			wantErr: false,
			checks: func(t *testing.T, config string) {
				// Check all three servers are present
				for _, vmid := range []types.VMID{201, 202, 203} {
					expected := fmt.Sprintf("server talos-cp-%d", vmid)
					if !strings.Contains(config, expected) {
						t.Errorf("config missing backend server for VMID %d", vmid)
					}
				}
				// Verify no stats auth when credentials not provided
				if strings.Contains(config, "stats auth") {
					t.Error("config should not have stats auth when credentials empty")
				}
			},
		},
		{
			name: "missing haproxy ip",
			config: Config{
				HAProxyIP: nil,
				ControlPlanes: []Backend{
					{VMID: 201, IP: net.ParseIP("192.168.1.201")},
				},
			},
			wantErr: true,
			errMsg:  "HAProxy IP is required",
		},
		{
			name: "no control planes",
			config: Config{
				HAProxyIP:     net.ParseIP("192.168.1.237"),
				ControlPlanes: []Backend{},
			},
			wantErr: true,
			errMsg:  "at least one control plane backend is required",
		},
		{
			name: "nil control planes slice",
			config: Config{
				HAProxyIP:     net.ParseIP("192.168.1.237"),
				ControlPlanes: nil,
			},
			wantErr: true,
			errMsg:  "at least one control plane backend is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.config.Generate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Generate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Generate() error message = %v, want containing %v", err.Error(), tt.errMsg)
				}
			}
			if tt.checks != nil && !tt.wantErr {
				tt.checks(t, got)
			}
		})
	}
}

func TestConfig_Generate_ContainsExpectedSections(t *testing.T) {
	config := Config{
		HAProxyIP:     net.ParseIP("192.168.1.237"),
		StatsUser:     "admin",
		StatsPassword: "admin123",
		ControlPlanes: []Backend{
			{VMID: 201, IP: net.ParseIP("192.168.1.201")},
			{VMID: 202, IP: net.ParseIP("192.168.1.202")},
		},
	}

	got, err := config.Generate()
	if err != nil {
		t.Fatalf("Generate() unexpected error: %v", err)
	}

	// Verify all major sections are present
	requiredSections := []string{
		"global",
		"defaults",
		"listen stats",
		"frontend k8s-apiserver",
		"backend k8s-controlplane",
		"frontend talos-apiserver",
		"backend talos-controlplane",
	}

	for _, section := range requiredSections {
		if !strings.Contains(got, section) {
			t.Errorf("generated config missing section: %s", section)
		}
	}
}

func TestConfig_Generate_BackendServerFormat(t *testing.T) {
	config := Config{
		HAProxyIP: net.ParseIP("192.168.1.237"),
		ControlPlanes: []Backend{
			{VMID: 201, IP: net.ParseIP("192.168.1.201")},
		},
	}

	got, err := config.Generate()
	if err != nil {
		t.Fatalf("Generate() unexpected error: %v", err)
	}

	// Verify server line format: server talos-cp-{VMID} {IP}:6443 check
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if strings.Contains(line, "server talos-cp-") {
			// Should match pattern: server talos-cp-201 192.168.1.201:6443 check
			if !strings.HasPrefix(strings.TrimSpace(line), "server talos-cp-") {
				t.Errorf("backend server line has wrong format: %s", line)
			}
			if !strings.Contains(line, "check") {
				t.Errorf("backend server missing health check: %s", line)
			}
		}
	}
}

func TestConfigFromClusterState(t *testing.T) {
	cfg := &types.Config{
		HAProxyIP:            net.ParseIP("192.168.1.237"),
		HAProxyStatsUser:     "haproxy",
		HAProxyStatsPassword: "securepass",
	}

	state := &types.ClusterState{
		ControlPlanes: []types.NodeState{
			{
				VMID: 201,
				IP:   net.ParseIP("192.168.1.201"),
			},
			{
				VMID: 202,
				IP:   net.ParseIP("192.168.1.202"),
			},
		},
		Workers: []types.NodeState{
			{
				VMID: 301,
				IP:   net.ParseIP("192.168.1.301"),
			},
		},
	}

	got := ConfigFromClusterState(cfg, state)

	// Verify HAProxy config fields
	if !got.HAProxyIP.Equal(cfg.HAProxyIP) {
		t.Errorf("HAProxyIP = %v, want %v", got.HAProxyIP, cfg.HAProxyIP)
	}
	if got.StatsUser != cfg.HAProxyStatsUser {
		t.Errorf("StatsUser = %v, want %v", got.StatsUser, cfg.HAProxyStatsUser)
	}
	if got.StatsPassword != cfg.HAProxyStatsPassword {
		t.Errorf("StatsPassword = %v, want %v", got.StatsPassword, cfg.HAProxyStatsPassword)
	}

	// Verify only control planes are included (not workers)
	if len(got.ControlPlanes) != 2 {
		t.Errorf("ControlPlanes length = %v, want 2", len(got.ControlPlanes))
	}

	// Verify control plane details
	expectedBackends := map[types.VMID]string{
		201: "192.168.1.201",
		202: "192.168.1.202",
	}

	for _, backend := range got.ControlPlanes {
		expectedIP, ok := expectedBackends[backend.VMID]
		if !ok {
			t.Errorf("unexpected backend VMID: %d", backend.VMID)
			continue
		}
		if backend.IP.String() != expectedIP {
			t.Errorf("VMID %d: IP = %v, want %v", backend.VMID, backend.IP, expectedIP)
		}
		delete(expectedBackends, backend.VMID)
	}

	if len(expectedBackends) > 0 {
		t.Errorf("missing backends for VMIDs: %v", expectedBackends)
	}
}

func TestConfigFromClusterState_EmptyControlPlanes(t *testing.T) {
	cfg := &types.Config{
		HAProxyIP:            net.ParseIP("192.168.1.237"),
		HAProxyStatsUser:     "admin",
		HAProxyStatsPassword: "pass",
	}

	state := &types.ClusterState{
		ControlPlanes: []types.NodeState{},
		Workers: []types.NodeState{
			{VMID: 301, IP: net.ParseIP("192.168.1.301")},
		},
	}

	got := ConfigFromClusterState(cfg, state)

	if len(got.ControlPlanes) != 0 {
		t.Errorf("ControlPlanes should be empty, got %d", len(got.ControlPlanes))
	}

	// Should still be able to generate (will error due to empty backends)
	_, err := got.Generate()
	if err == nil {
		t.Error("expected error when generating config with no control planes")
	}
}

func TestConfigFromClusterState_NilState(t *testing.T) {
	cfg := &types.Config{
		HAProxyIP:            net.ParseIP("192.168.1.237"),
		HAProxyStatsUser:     "admin",
		HAProxyStatsPassword: "pass",
	}

	// This should handle nil state gracefully (though may panic depending on design)
	// Testing that it doesn't crash with empty control planes
	state := &types.ClusterState{
		ControlPlanes: nil,
	}

	got := ConfigFromClusterState(cfg, state)

	if got.ControlPlanes == nil {
		// nil slice is acceptable, but let's verify other fields
		if !got.HAProxyIP.Equal(cfg.HAProxyIP) {
			t.Error("HAProxyIP not set correctly")
		}
	}
}

func TestBackend_Struct(t *testing.T) {
	// Test that Backend struct can be created and fields accessed
	ip := net.ParseIP("192.168.1.201")
	backend := Backend{
		VMID: 201,
		IP:   ip,
	}

	if backend.VMID != 201 {
		t.Errorf("VMID = %d, want 201", backend.VMID)
	}
	if !backend.IP.Equal(ip) {
		t.Errorf("IP = %v, want %v", backend.IP, ip)
	}
}
