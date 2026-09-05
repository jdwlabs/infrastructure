# Forked build of the official iscsi-tools system extension. The only change
# is baking /etc/iscsi/iscsid.conf into the ext-iscsid service — see
# iscsid.conf for the timeout rationale and iscsid.yaml for the mount
# arrangement that makes the file reachable.
#
# Why a fork rather than runtime configuration: both runtime mechanisms are
# proven broken on these nodes, live. ExtensionServiceConfig injects its bind
# mount after the service spec has already bind-mounted host /etc/iscsi
# read-only, so the runtime cannot create the file mountpoint and ext-iscsid
# restart-loops with no working initiator on the node. machine.files only
# permits creating files under /var; targeting /etc/iscsi/iscsid.conf fails
# writeUserFiles at boot, which blocks the cri service from registering and
# the node from joining the cluster at all. Baking the file into the
# extension image needs no runtime write, so neither failure mode exists.
#
# The base is pinned to the exact digest the Image Factory extensions bundle
# for Talos v1.13.4 resolves for siderolabs/iscsi-tools — the same bits the
# fleet runs today — not the floating v0.2.0 tag, which has been re-pushed
# since and no longer matches.
FROM ghcr.io/siderolabs/iscsi-tools:v0.2.0@sha256:3a121a4451e746fe50eaee4089429814fdc199ad90758fd11958b3340c70e8f9

COPY manifest.yaml /manifest.yaml
COPY iscsid.yaml /rootfs/usr/local/etc/containers/iscsid.yaml
COPY iscsid.conf /rootfs/usr/local/etc/iscsi/iscsid.conf
COPY initiatorname.iscsi /rootfs/usr/local/etc/iscsi/initiatorname.iscsi
