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
# The base is the digest the Image Factory extensions bundle resolves for the
# TALOS_VERSION the extension workflow builds against — the same bits the
# fleet runs — never whatever the v0.2.0 tag currently points at, which
# upstream re-pushes for each Talos release and which therefore names a
# different Talos version's build most of the time. Moving TALOS_VERSION
# means re-deriving this digest; the workflow fails the build when the two
# disagree and prints the value to paste.
FROM ghcr.io/siderolabs/iscsi-tools:v0.2.0@sha256:12521145e65403037a7412bf9abce1efd08583e018e5eed8e1c5b9faf52d1da4

COPY manifest.yaml /manifest.yaml
COPY iscsid.yaml /rootfs/usr/local/etc/containers/iscsid.yaml
COPY iscsid.conf /rootfs/usr/local/etc/iscsi/iscsid.conf
COPY initiatorname.iscsi /rootfs/usr/local/etc/iscsi/initiatorname.iscsi
