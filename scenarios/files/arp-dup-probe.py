#!/usr/bin/env python3
"""Duplicate-address probe for hosts without arping(8).

Sends the RFC 5227 ARP probe that `arping -D` sends (sender 0.0.0.0, broadcast)
and reports every MAC that claims the address. Exit 0 means no address was
claimed; exit 1 means at least one was. Needs root (raw socket) and reads only:
it changes no address, neighbour entry or setting on the host.

    python3 arp-dup-probe.py <interface> <probes> <ip> [<ip> ...]

Always include one address you know is in use as a positive control; a probe
that cannot see the gateway cannot vouch for an empty answer.
"""
import fcntl
import select
import socket
import struct
import sys
import time

SIOCGIFHWADDR = 0x8927
ETH_P_ARP = 0x0806


def fmt(mac):
    return ":".join("%02x" % b for b in mac)


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    iface, probes, targets = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ARP))
    sock.bind((iface, 0))
    own = fcntl.ioctl(sock.fileno(), SIOCGIFHWADDR, struct.pack("256s", iface.encode()[:15]))[18:24]

    claimed = False
    for ip in targets:
        target = socket.inet_aton(ip)
        probe = (
            b"\xff" * 6 + own + b"\x08\x06"
            + struct.pack("!HHBBH", 1, 0x0800, 6, 4, 1)
            + own + b"\0\0\0\0" + b"\0" * 6 + target
        )
        claimants = set()
        for _ in range(probes):
            sock.send(probe)
            deadline = time.time() + 1.2
            while time.time() < deadline:
                ready, _, _ = select.select([sock], [], [], max(0, deadline - time.time()))
                if not ready:
                    break
                frame = sock.recv(2048)
                if len(frame) < 42 or frame[6:12] == own:
                    continue
                opcode = struct.unpack("!H", frame[20:22])[0]
                # A reply names the address as its sender; another host probing
                # the same address (sender 0.0.0.0, target ours) also counts.
                if opcode == 2 and frame[28:32] == target:
                    claimants.add(fmt(frame[6:12]))
                elif opcode == 1 and frame[28:32] == b"\0\0\0\0" and frame[38:42] == target:
                    claimants.add(fmt(frame[6:12]))
        print("%s: %d probes on %s, %d claimant(s) %s" % (ip, probes, iface, len(claimants), sorted(claimants)))
        claimed = claimed or bool(claimants)
    sys.exit(1 if claimed else 0)


if __name__ == "__main__":
    main()
