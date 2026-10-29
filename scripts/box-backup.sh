#!/bin/bash
# Backs up box with restic: the state (database, keys, host CA, config
# archives, /etc/exe) and every VM's disk as a raw image. Optional: does
# nothing unless /etc/exe/backup.conf exists (see backup.conf.example).
# Disks are crash-consistent, like after a power cut. They are read as bytes
# from a temporary clone, never mounted: exe-host does not parse VM data.
# Raw images, not `zfs send`: send streams carry the snapshot id in every
# record, so restic could not deduplicate one night against the next.
set -euo pipefail
CONF=/etc/exe/backup.conf
if [ ! -r $CONF ]; then echo "no $CONF: backups are off"; exit 0; fi
set -a; . $CONF; set +a
DATASET=${BOX_DATASET:-exe/vms}
TMP=${DATASET%/*}/backup-tmp
KEEP=${BOX_BACKUP_KEEP:---keep-daily 7 --keep-weekly 4 --keep-monthly 3}
TAG=backup-$(date -u +%Y%m%dT%H%M%SZ)
work=$(mktemp -d)
cleanup() {
	rm -rf "$work"
	zfs destroy -r "$TMP" 2>/dev/null || true
	zfs list -H -t snapshot -o name -r "$DATASET" | grep "@$TAG\$" | xargs -r -n1 zfs destroy || true
}
trap cleanup EXIT

restic cat config >/dev/null 2>&1 || restic init

/usr/local/sbin/boxd -dump-db "$work/box.db"
restic backup --host exe-host --tag state /etc/exe "$work/box.db" \
	/var/lib/exe --exclude '/var/lib/exe/box.db*' --exclude /var/lib/exe/kernels

zfs create -o mountpoint=none "$TMP"
failed=0
for ds in $(zfs list -H -o name -d 1 "$DATASET" | tail -n +2); do
	vm=${ds##*/}
	zfs snapshot "$ds@$TAG"
	zfs clone "$ds@$TAG" "$TMP/$vm"
	udevadm settle
	if ! restic backup --host exe-host --tag vm --tag "vm:$vm" --stdin-filename "$vm.img" \
		--stdin-from-command -- cat "/dev/zvol/$TMP/$vm"; then
		echo "backup of $vm failed" >&2
		failed=1
	fi
	zfs destroy "$TMP/$vm"
	zfs destroy "$ds@$TAG"
done

restic forget --host exe-host --group-by host,tags --prune $KEEP
date -u +%s > /var/lib/exe/backup-last
exit $failed
