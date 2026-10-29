#!/bin/bash
# Builds boxd and installs it on an exe-host. Every version stays in
# /usr/local/lib/boxd; /usr/local/sbin/boxd links to the current one and is
# switched atomically. If boxd does not come back healthy, the previous
# version is put back.
# Usage: scripts/deploy.sh <ssh-target>            e.g. me@exe-host
#        scripts/deploy.sh <ssh-target> --rollback
set -euo pipefail
HOST=${1:?usage: deploy.sh <ssh-target> [--rollback]}
cd "$(dirname "$0")/.."

remote() { ssh "$HOST" "sudo bash -s" -- "$@"; }

if [ "${2:-}" = --rollback ]; then
	remote <<'SH'
set -euo pipefail
cur=$(readlink -f /usr/local/sbin/boxd)
prev=$(ls -1t /usr/local/lib/boxd/boxd-* | grep -vx "$cur" | head -1)
ln -sfn "$prev" /usr/local/sbin/boxd.new && mv -T /usr/local/sbin/boxd.new /usr/local/sbin/boxd
systemctl restart boxd
echo "rolled back to ${prev##*/}"
SH
	exit
fi

VERSION=$(date -u +%Y%m%d%H%M%S)-$(git rev-parse --short HEAD 2>/dev/null || echo dev)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "dist/boxd-$VERSION" ./cmd/boxd
scp -q "dist/boxd-$VERSION" "$HOST:/tmp/"
remote "$VERSION" <<'SH'
set -euo pipefail
V=$1 LIB=/usr/local/lib/boxd
mkdir -p $LIB
install -m755 /tmp/boxd-$V $LIB/boxd-$V && rm /tmp/boxd-$V
# The first time, the binary in place becomes a version too.
if [ -f /usr/local/sbin/boxd ] && [ ! -L /usr/local/sbin/boxd ]; then
	mv /usr/local/sbin/boxd $LIB/boxd-before-deploy-script
fi
prev=$(readlink -f /usr/local/sbin/boxd 2>/dev/null || true)
switch() { ln -sfn "$1" /usr/local/sbin/boxd.new && mv -T /usr/local/sbin/boxd.new /usr/local/sbin/boxd; }
switch $LIB/boxd-$V
systemctl restart boxd
healthy() {
	for _ in $(seq 20); do
		sleep 1
		systemctl is-active -q boxd || continue
		# Any HTTP answer: the web front end is up.
		[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/)" != 000 ] && return 0
	done
	return 1
}
if ! healthy; then
	echo "boxd $V is not healthy: rolling back" >&2
	journalctl -u boxd -n 20 --no-pager >&2
	if [ -n "$prev" ]; then switch "$prev"; systemctl restart boxd; fi
	exit 1
fi
# Keep the last five versions.
ls -1t $LIB/boxd-* | tail -n +6 | xargs -r rm
echo "boxd $V running"
SH
