package udpnat

import (
	"net"
	"strings"
	"testing"
)

func TestGenerateSortsAndRendersRules(t *testing.T) {
	c := &Config{
		TargetIP: net.ParseIP("192.168.1.163"),
		Forwards: []Forward{
			{Name: "ns/second", ExternalPort: 19133, NodePort: 31133},
			{Name: "ns/first", ExternalPort: 19132, NodePort: 31132},
		},
	}
	out, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "udp dport 19132 dnat to 192.168.1.163:31132") {
		t.Errorf("missing dnat rule for 19132:\n%s", out)
	}
	if !strings.Contains(out, "ip daddr 192.168.1.163 udp dport 31132 masquerade") {
		t.Errorf("missing masquerade rule for 31132:\n%s", out)
	}
	// Sorted output is what makes a diff against the live host meaningful.
	if strings.Index(out, "dport 19132 dnat") > strings.Index(out, "dport 19133 dnat") {
		t.Errorf("forwards not sorted by external port:\n%s", out)
	}
	// Flushing would drop Tailscale's rules on this host. Check for an actual
	// directive rather than the substring, which also appears in the header
	// comment explaining why the file must not flush.
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "flush") {
			panic("generated ruleset must never flush; it would drop Tailscale's rules")
		}
	}
	// Both nat hooks must be valid nftables hook names.
	if !strings.Contains(out, "type nat hook prerouting priority dstnat") {
		t.Errorf("prerouting chain malformed:\n%s", out)
	}
	if !strings.Contains(out, "type nat hook postrouting priority srcnat") {
		t.Errorf("postrouting chain malformed - 'srcnat' is a priority, not a hook:\n%s", out)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	mk := func(f []Forward) string {
		c := &Config{TargetIP: net.ParseIP("192.168.1.163"), Forwards: f}
		out, err := c.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		return out
	}
	a := mk([]Forward{{Name: "a", ExternalPort: 19132, NodePort: 31132}, {Name: "b", ExternalPort: 19140, NodePort: 31140}})
	b := mk([]Forward{{Name: "b", ExternalPort: 19140, NodePort: 31140}, {Name: "a", ExternalPort: 19132, NodePort: 31132}})
	if a != b {
		t.Error("same forwards in different order produced different output")
	}
}

func TestGenerateRejectsDuplicateExternalPort(t *testing.T) {
	c := &Config{
		TargetIP: net.ParseIP("192.168.1.163"),
		Forwards: []Forward{
			{Name: "ns/a", ExternalPort: 19132, NodePort: 31132},
			{Name: "ns/b", ExternalPort: 19132, NodePort: 31133},
		},
	}
	_, err := c.Generate()
	if err == nil {
		t.Fatal("expected an error when two services claim the same external port")
	}
	if !strings.Contains(err.Error(), "19132") {
		t.Errorf("error should name the contested port, got: %v", err)
	}
}

func TestGenerateRequiresTargetIP(t *testing.T) {
	c := &Config{Forwards: []Forward{{Name: "a", ExternalPort: 19132, NodePort: 31132}}}
	if _, err := c.Generate(); err == nil {
		t.Fatal("expected an error when TargetIP is unset")
	}
}

func TestGenerateRejectsOutOfRangePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Forward
	}{
		{"external too high", Forward{Name: "a", ExternalPort: 70000, NodePort: 31132}},
		{"node port zero", Forward{Name: "a", ExternalPort: 19132, NodePort: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{TargetIP: net.ParseIP("192.168.1.163"), Forwards: []Forward{tc.f}}
			if _, err := c.Generate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// Pins the generator to the rules hand-applied on the HAProxy VM when Bedrock
// was first published, so the first generated run is a no-op rather than a
// surprise change to a live path.
func TestGenerateMatchesRulesAppliedByHand(t *testing.T) {
	c := &Config{
		TargetIP: net.ParseIP("192.168.1.163"),
		Forwards: []Forward{{Name: "jdwillmsen-prd/minecraft-fwb", ExternalPort: 19132, NodePort: 31132}},
	}
	out, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, want := range []string{
		"table ip minecraft",
		"delete table ip minecraft",
		"type nat hook prerouting priority dstnat; policy accept;",
		"udp dport 19132 dnat to 192.168.1.163:31132",
		"type nat hook postrouting priority srcnat; policy accept;",
		"ip daddr 192.168.1.163 udp dport 31132 masquerade",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated ruleset missing %q:\n%s", want, out)
		}
	}
}
