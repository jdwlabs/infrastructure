// Package sshutil holds SSH helpers shared by the packages that dial Proxmox
// hosts.
package sshutil

import (
	"os"
	"path/filepath"

	"github.com/skeema/knownhosts"
)

// HostKeyAlgorithms returns the host-key algorithms trusted for addr in the
// user's known_hosts file, most preferred first, for use as
// ssh.ClientConfig.HostKeyAlgorithms.
//
// Without this, Go negotiates from its own default preference order, which is
// independent of what the host is actually trusted under. A host keyscanned for
// only one key type then presents a key of a different type, the callback finds
// no matching entry, and the dial fails as "knownhosts: key mismatch" -- the
// wording for a changed host key, so it reads as a compromise rather than as an
// incomplete known_hosts entry.
//
// addr must carry the port ("host:22"), matching the form ssh.Dial is given.
//
// An empty result means the host has no entry yet, and the caller should leave
// HostKeyAlgorithms unset so the dial can proceed under Go's defaults -- the
// host key callback still decides whether to trust what comes back.
func HostKeyAlgorithms(addr string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	return hostKeyAlgorithmsFrom(filepath.Join(home, ".ssh", "known_hosts"), addr)
}

// hostKeyAlgorithmsFrom is HostKeyAlgorithms against an explicit known_hosts
// path, so tests can supply a fixture instead of the caller's real file.
func hostKeyAlgorithmsFrom(khPath, addr string) []string {
	kh, err := knownhosts.New(khPath)
	if err != nil {
		return nil
	}

	return kh.HostKeyAlgorithms(addr)
}
