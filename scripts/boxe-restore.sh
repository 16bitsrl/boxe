#!/bin/bash
# Puts back the disk of a VM from the backups (see boxe-backup.sh).
# Usage: sudo boxe-restore.sh <vm> [restic snapshot id, default: latest]
# The VM must exist in boxed (after a full disaster recovery, restore the
# state first: docs/operations.md). Its current disk is kept as
# <dataset>/<vm>-before-restore until you delete it.
set -euo pipefail
VM=$1 SNAP=${2:-latest}
set -a; . /etc/boxe/backup.conf; set +a
DATASET=${BOXE_DATASET:-boxe/vms}
DS=$DATASET/$VM

size=$(restic ls --json --host boxe --tag "vm:$VM" "$SNAP" "/$VM.img" |
	python3 -c 'import json,sys
for l in sys.stdin:
    d = json.loads(l)
    if d.get("type") == "file": print(d["size"])')
[ -n "$size" ] || { echo "no backup of $VM" >&2; exit 1; }
systemctl stop "boxe-vm-$VM" 2>/dev/null || true
if zfs list "$DS" >/dev/null 2>&1; then
	zfs rename "$DS" "$DS-before-restore"
fi
zfs create -s -V "$size" -o volblocksize=16k "$DS"
udevadm settle
# conv=sparse: zeros stay holes, the disk takes only the space it uses.
restic dump --host boxe --tag "vm:$VM" "$SNAP" "/$VM.img" | dd of="/dev/zvol/$DS" bs=1M conv=sparse status=none
echo "restored $DS; start it with: ssh boxe restart $VM"
