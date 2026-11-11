#!/usr/bin/env python3
"""Gratuitous ARP for hosts without arping(8).

`ip addr add` puts an address on an interface without telling the LAN, so
neighbours keep the previous owner's MAC for it until their cache entry
expires. This broadcasts an unsolicited ARP reply for <ip> from <interface>'s
own MAC, which refreshes them at once. Run it only on the host that now owns
the address.

    python3 arp-announce.py <interface> <ip> [<count>]
"""
import fcntl
import socket
import struct
import sys
import time

SIOCGIFHWADDR = 0x8927
ETH_P_ARP = 0x0806


def frame(mac, ip):
    addr = socket.inet_aton(ip)
    return (
        b"\xff" * 6 + mac + b"\x08\x06"
        + struct.pack("!HHBBH", 1, 0x0800, 6, 4, 2)
        + mac + addr + b"\xff" * 6 + addr
    )


def main():
    if len(sys.argv) not in (3, 4):
        sys.exit(__doc__)
    iface, ip = sys.argv[1], sys.argv[2]
    count = int(sys.argv[3]) if len(sys.argv) == 4 else 3
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ARP))
    sock.bind((iface, 0))
    mac = fcntl.ioctl(sock.fileno(), SIOCGIFHWADDR, struct.pack("256s", iface.encode()[:15]))[18:24]
    for _ in range(count):
        sock.send(frame(mac, ip))
        time.sleep(0.5)
    print("announced %s is-at %s x%d on %s" % (ip, ":".join("%02x" % b for b in mac), count, iface))


if __name__ == "__main__":
    main()
