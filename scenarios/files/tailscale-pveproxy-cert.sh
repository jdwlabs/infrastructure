#!/usr/bin/env bash
#
# Refresh this Proxmox node's Tailscale-issued TLS certificate and hand it to
# pveproxy. Installed on every hypervisor as /usr/local/sbin and driven by the
# companion systemd timer; see scenarios/proxmox-tailscale-tls.md.
#
# This script exists because `tailscale cert` does not renew anything on its
# own once it has written files to disk — tailscaled has no idea where those
# files went, so nothing refreshes them. The certificates are 90-day Let's
# Encrypt certificates, so without a periodic re-run the web UI silently goes
# back to an expired-certificate warning one quarter after it was set up.

set -euo pipefail

# The tailnet's MagicDNS suffix. Tailscale names a node after its hostname, so
# a node's certificate domain is <short-hostname>.<suffix> unless a name
# collision forced Tailscale to append a numeric suffix at join time — which is
# why the FQDN can also be passed explicitly as the first argument.
readonly TAILNET_DOMAIN="tail5bbd6f.ts.net"

# Renew once under 30 days remain. Let's Encrypt certificates last 90 days, so
# a daily timer gets roughly 30 attempts before expiry — enough slack for the
# tailnet, Let's Encrypt, or this host to be unavailable for a stretch without
# the certificate lapsing.
readonly MIN_VALIDITY="720h"

readonly FQDN="${1:-$(hostname -s).${TAILNET_DOMAIN}}"

# /etc/pve/local is a per-node symlink into the cluster filesystem, so each
# host keeps its own certificate here with no risk of overwriting a peer's.
readonly LIVE_CERT="/etc/pve/local/pveproxy-ssl.pem"
readonly LIVE_KEY="/etc/pve/local/pveproxy-ssl.key"

# Stage on tmpfs: a failed or partial fetch must never be able to leave stale
# key material on disk, and a reboot clears anything left behind.
readonly STAGE_DIR="/run/tailscale-pveproxy-cert"

install -d -m 0700 "${STAGE_DIR}"
trap 'rm -rf "${STAGE_DIR}"' EXIT

# tailscale reuses its cached certificate when it still has more than
# MIN_VALIDITY left, so this is cheap on the days nothing needs renewing.
# A failure here aborts the script before anything live is touched.
tailscale cert \
    --min-validity="${MIN_VALIDITY}" \
    --cert-file="${STAGE_DIR}/cert.pem" \
    --key-file="${STAGE_DIR}/key.pem" \
    "${FQDN}"

if [ -f "${LIVE_CERT}" ] && [ -f "${LIVE_KEY}" ] &&
    cmp -s "${STAGE_DIR}/cert.pem" "${LIVE_CERT}" &&
    cmp -s "${STAGE_DIR}/key.pem" "${LIVE_KEY}"; then
    echo "certificate for ${FQDN} unchanged; pveproxy left alone"
    exit 0
fi

# The cluster filesystem sets ownership and mode itself (root:www-data 0640),
# so writing the contents is the whole install — no chown/chmod to get wrong.
#
# pveproxy only uses the custom pair when both files are present, and it reads
# them once at startup. Writing the key first means a restart racing this write
# falls back to the built-in certificate rather than serving a mismatched pair.
cat "${STAGE_DIR}/key.pem" >"${LIVE_KEY}"
cat "${STAGE_DIR}/cert.pem" >"${LIVE_CERT}"

echo "installed certificate for ${FQDN}; reloading pveproxy"
systemctl reload pveproxy
