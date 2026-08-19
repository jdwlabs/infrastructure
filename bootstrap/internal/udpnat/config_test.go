package udpnat

import (
	"net"
	"os"
	"path/filepath"
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
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "flush") {
			t.Fatalf("generated ruleset must never flush; it would drop Tailscale's rules:\n%s", out)
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

// An IPv6 target renders cleanly and is rejected by nft on the host, which
// moves the failure to apply time on a live path.
func TestGenerateRequiresIPv4Target(t *testing.T) {
	c := &Config{
		TargetIP: net.ParseIP("fd00::1"),
		Forwards: []Forward{{Name: "a", ExternalPort: 19132, NodePort: 31132}},
	}
	if _, err := c.Generate(); err == nil {
		t.Fatal("expected an error when TargetIP is IPv6")
	}
}

func TestGenerateRejectsOutOfRangePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Forward
	}{
		{"external above the forwarded range", Forward{Name: "a", ExternalPort: 19142, NodePort: 31142}},
		{"external below the forwarded range", Forward{Name: "a", ExternalPort: 19131, NodePort: 31131}},
		// Valid as a port number, and unreachable: the router forwards only
		// the published range, so rendering this would be a silent dead end.
		{"external a valid port outside the forwarded range", Forward{Name: "a", ExternalPort: 19200, NodePort: 31200}},
		{"node port zero", Forward{Name: "a", ExternalPort: 19132, NodePort: 0}},
		{"node port too high", Forward{Name: "a", ExternalPort: 19132, NodePort: 70000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{TargetIP: net.ParseIP("192.168.1.163"), Forwards: []Forward{tc.f}}
			if _, err := c.Generate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// Pins the full rendered ruleset byte for byte. The rules are applied to a live
// host, so a diff against the file that is actually shipped is the only check
// that catches an accidental change to the header, the table, or rule ordering.
// Regenerate deliberately, never to make a failing test pass.
func TestGenerateMatchesGolden(t *testing.T) {
	c := &Config{
		TargetIP: net.ParseIP("192.168.1.163"),
		Forwards: []Forward{{Name: "jdwillmsen-prd/minecraft-fwb", ExternalPort: 19132, NodePort: 31132}},
	}
	out, err := c.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "single-forward.nft"))
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	if out != string(want) {
		t.Errorf("rendered ruleset does not match testdata/single-forward.nft\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
}

// The bounds are a published contract with the router rule, not an internal
// detail: a reader changing one must know to change the other.
func TestExternalPortRangeMatchesTheRouterRule(t *testing.T) {
	if ExternalPortMin != 19132 || ExternalPortMax != 19141 {
		t.Errorf("range is %d-%d; the router forwards 19132-19141 and both must agree",
			ExternalPortMin, ExternalPortMax)
	}
}
