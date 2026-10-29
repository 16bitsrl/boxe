#!/bin/bash
# Builds boxed and installs it on a host. Every version stays in
# /usr/local/lib/boxed; /usr/local/sbin/boxed links to the current one and is
# switched atomically. If boxed does not come back healthy, the previous
# version is put back.
# Usage: scripts/deploy.sh <ssh-target>            e.g. me@my-host
#        scripts/deploy.sh <ssh-target> --rollback
set -euo pipefail
HOST=${1:?usage: deploy.sh <ssh-target> [--rollback]}
cd "$(dirname "$0")/.."

remote() { ssh "$HOST" "sudo bash -s" -- "$@"; }

if [ "${2:-}" = --rollback ]; then
	remote <<'SH'
set -euo pipefail
cur=$(readlink -f /usr/local/sbin/boxed)
prev=$(ls -1t /usr/local/lib/boxed/boxed-* | grep -vx "$cur" | head -1)
ln -sfn "$prev" /usr/local/sbin/boxed.new && mv -T /usr/local/sbin/boxed.new /usr/local/sbin/boxed
systemctl restart boxed
echo "rolled back to ${prev##*/}"
SH
	exit
fi

VERSION=$(date -u +%Y%m%d%H%M%S)-$(git rev-parse --short HEAD 2>/dev/null || echo dev)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "dist/boxed-$VERSION" ./cmd/boxed
scp -q "dist/boxed-$VERSION" "$HOST:/tmp/"
remote "$VERSION" <<'SH'
set -euo pipefail
V=$1 LIB=/usr/local/lib/boxed
mkdir -p $LIB
install -m755 /tmp/boxed-$V $LIB/boxed-$V && rm /tmp/boxed-$V
# The first time, the binary in place becomes a version too.
if [ -f /usr/local/sbin/boxed ] && [ ! -L /usr/local/sbin/boxed ]; then
	mv /usr/local/sbin/boxed $LIB/boxed-before-deploy-script
fi
prev=$(readlink -f /usr/local/sbin/boxed 2>/dev/null || true)
switch() { ln -sfn "$1" /usr/local/sbin/boxed.new && mv -T /usr/local/sbin/boxed.new /usr/local/sbin/boxed; }
switch $LIB/boxed-$V
systemctl restart boxed
healthy() {
	for _ in $(seq 20); do
		sleep 1
		systemctl is-active -q boxed || continue
		# Any HTTP answer: the web front end is up.
		[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/)" != 000 ] && return 0
	done
	return 1
}
if ! healthy; then
	echo "boxed $V is not healthy: rolling back" >&2
	journalctl -u boxed -n 20 --no-pager >&2
	if [ -n "$prev" ]; then switch "$prev"; systemctl restart boxed; fi
	exit 1
fi
# Keep the last five versions.
ls -1t $LIB/boxed-* | tail -n +6 | xargs -r rm
echo "boxed $V running"
SH
