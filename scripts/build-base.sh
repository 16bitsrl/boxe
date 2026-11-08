#!/bin/bash
# Exports a container image to an ext4 zvol and takes its @base snapshot.
# Usage: sudo build-base.sh <image> <dataset-name> [size]
# e.g.   sudo build-base.sh boxeuntu:20261029 boxeuntu-20261029
# A name with a slash is the whole dataset (boxed passes it so), else it goes
# under boxe/images. boxed runs it, installed as /usr/local/sbin/boxe-build-base,
# for the official images it downloads.
# Names are never reused: VMs are clones of the snapshot, so an image in use
# cannot be rebuilt in place. On failure nothing is left behind.
set -euo pipefail
IMAGE=$1 NAME=$2 SIZE=${3:-20G}
DS=boxe/images/$NAME
case $NAME in */*) DS=$NAME ;; esac
DEV=/dev/zvol/$DS MNT=$(mktemp -d)
CID=""

if zfs list "$DS" >/dev/null 2>&1; then
	echo "$DS already exists: pick a new name" >&2
	exit 1
fi
cleanup() {
	status=$?
	[ -n "$CID" ] && docker rm "$CID" >/dev/null 2>&1
	mountpoint -q "$MNT" && umount "$MNT"
	rmdir "$MNT" 2>/dev/null
	if [ $status -ne 0 ]; then
		zfs destroy -r "$DS" 2>/dev/null
		echo "failed: $DS removed" >&2
	fi
}
trap cleanup EXIT
# Stopped (boxed's deadline, Ctrl-C): clean up as after a failure.
trap 'exit 143' TERM INT

zfs create -s -V "$SIZE" -o volblocksize=16k "$DS"
udevadm settle
mkfs.ext4 -q -L root "$DEV"
mount "$DEV" "$MNT"
CID=$(docker create "$IMAGE")
docker export "$CID" | tar -xpf - -C "$MNT" --numeric-owner
# Without this, systemd thinks it runs in a Docker container
rm -f "$MNT/.dockerenv"
# docker export drops file capabilities
setcap cap_net_raw=+ep "$MNT/usr/bin/ping"
umount "$MNT"
zfs snapshot "$DS@base"
# The boxe the image was built from, for ssh boxe images
SOURCE=$(docker inspect -f '{{index .Config.Labels "boxe.source"}}' "$IMAGE")
[ -n "$SOURCE" ] && zfs set boxe:source="$SOURCE" "$DS@base"
zfs list -o name,used,refer,volsize "$DS"
