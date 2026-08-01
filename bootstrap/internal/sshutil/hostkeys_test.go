package sshutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Throwaway keypairs generated for this test and discarded. They have to be
// well-formed: knownhosts abandons the whole file on an unparseable line, so an
// invented blob would make every case return empty and the tests pass for the
// wrong reason.
const (
	ed25519Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFyBinF0uloR0eNH9BfTC1mm1xoSQQ0r+xLFaVLkTIcI"
	ecdsaKey   = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFQZcAAkPtQCYbhSWZr/kScft+07DkUDW9o0GJEjSvoH4efcisTYfq/f0t+r5Awwhl8SjpIO2jZaJgnCQi/UUyA="
)

func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestHostKeyAlgorithmsReflectsTrustedTypes(t *testing.T) {
	tests := []struct {
		name    string
		lines   []string
		addr    string
		want    []string
		wantAny bool
	}{
		{
			// The JDWLABS-267 shape: a host keyscanned for one type only. Before
			// this helper existed the dial negotiated ecdsa anyway and failed as
			// "key mismatch", which reads as a changed host key.
			name:  "single trusted type returns only that type",
			lines: []string{"[192.168.1.169]:22 " + ed25519Key},
			addr:  "192.168.1.169:22",
			want:  []string{"ssh-ed25519"},
		},
		{
			name: "host trusted for two types returns both",
			lines: []string{
				"[192.168.1.200]:22 " + ed25519Key,
				"[192.168.1.200]:22 " + ecdsaKey,
			},
			addr:    "192.168.1.200:22",
			wantAny: true,
		},
		{
			// Empty is the signal to leave HostKeyAlgorithms unset so the dial
			// proceeds under Go's defaults and the callback decides. Returning a
			// list here would pin a host we know nothing about.
			name:  "unknown host returns empty",
			lines: []string{"[192.168.1.200]:22 " + ed25519Key},
			addr:  "192.168.1.999:22",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostKeyAlgorithmsFrom(writeKnownHosts(t, tt.lines...), tt.addr)

			if tt.wantAny {
				if len(got) < 2 {
					t.Fatalf("want at least 2 algorithms for a host trusted twice, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for _, w := range tt.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing %q in %v", w, got)
				}
			}
		})
	}
}

func TestHostKeyAlgorithmsMissingFileIsNotFatal(t *testing.T) {
	// A first-run machine has no known_hosts. That must degrade to "no opinion",
	// not to a panic or a pinned-but-wrong list.
	got := hostKeyAlgorithmsFrom(filepath.Join(t.TempDir(), "absent"), "192.168.1.200:22")
	if got != nil {
		t.Fatalf("want nil for a missing known_hosts, got %v", got)
	}
}
