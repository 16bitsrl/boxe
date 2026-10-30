#!/bin/bash
# Builds boxed and installs it on a host. Every version stays in
# /usr/local/lib/boxed; /usr/local/sbin/boxed links to the current one and is
# switched atomically. If boxed does not come back healthy, the previous
# version is put back.
# Only boxed restarts: boxe-front, which holds the users' SSH and web
# connections, keeps running its version. --front restarts it too, on the
# new version, which drops those connections.
# Usage: scripts/deploy.sh <ssh-target>            e.g. me@my-host
#        scripts/deploy.sh <ssh-target> --front
#        scripts/deploy.sh <ssh-target> --rollback [--front]
# --rollback puts the previous version back and restarts boxed; with --front
# the front too (dropping the connections), for a bad version of the front.
set -euo pipefail
HOST=${1:?usage: deploy.sh <ssh-target> [--front|--rollback [--front]]}
FRONT=0
[ "${2:-}" = --front ] || [ "${3:-}" = --front ] && FRONT=1
cd "$(dirname "$0")/.."

remote() { ssh "$HOST" "sudo bash -s" -- "$@"; }

if [ "${2:-}" = --rollback ]; then
	remote "$FRONT" <<'SH'
set -euo pipefail
FRONT=$1
cur=$(readlink -f /usr/local/sbin/boxed)
prev=$(ls -1t /usr/local/lib/boxed/boxed-* | grep -vx "$cur" | head -1)
ln -sfn "$prev" /usr/local/sbin/boxed.new && mv -T /usr/local/sbin/boxed.new /usr/local/sbin/boxed
systemctl restart boxed
if [ "$FRONT" = 1 ]; then
	systemctl restart boxe-front
	echo "rolled back to ${prev##*/}, front included"
else
	echo "rolled back to ${prev##*/} (the front keeps its version: --rollback --front for it too)"
fi
SH
	exit
fi

VERSION=$(date -u +%Y%m%d%H%M%S)-$(git rev-parse --short HEAD 2>/dev/null || echo dev)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "dist/boxed-$VERSION" ./cmd/boxed
scp -q "dist/boxed-$VERSION" scripts/boxe-front.service "$HOST:/tmp/"
remote "$VERSION" "$FRONT" <<'SH'
set -euo pipefail
V=$1 FRONT=$2 LIB=/usr/local/lib/boxed
# The front's unit, kept up to date; it starts with boxed once boxed.service
# has -sockets (README), and restarts only with --front.
if ! cmp -s /tmp/boxe-front.service /etc/systemd/system/boxe-front.service; then
	install -m644 /tmp/boxe-front.service /etc/systemd/system/boxe-front.service
	systemctl daemon-reload
fi
rm /tmp/boxe-front.service
if [ "$FRONT" = 1 ] && ! systemctl cat boxed | grep -q -- -sockets; then
	echo "--front: boxed.service has no -sockets, install it from scripts/ first (README)" >&2
	exit 1
fi
restart() {
	if [ "$FRONT" = 1 ]; then systemctl restart boxe-front; fi
	systemctl restart boxed
}
mkdir -p $LIB
install -m755 /tmp/boxed-$V $LIB/boxed-$V && rm /tmp/boxed-$V
# The first time, the binary in place becomes a version too.
if [ -f /usr/local/sbin/boxed ] && [ ! -L /usr/local/sbin/boxed ]; then
	mv /usr/local/sbin/boxed $LIB/boxed-before-deploy-script
fi
prev=$(readlink -f /usr/local/sbin/boxed 2>/dev/null || true)
switch() { ln -sfn "$1" /usr/local/sbin/boxed.new && mv -T /usr/local/sbin/boxed.new /usr/local/sbin/boxed; }
switch $LIB/boxed-$V
restart
healthy() {
	for _ in $(seq 20); do
		sleep 1
		systemctl is-active -q boxed || continue
		# Any HTTP answer from boxed: the web front end is up. 502: the
		# front answered, boxed did not.
		case "$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/)" in
		000 | 502) ;;
		*) return 0 ;;
		esac
	done
	return 1
}
if ! healthy; then
	echo "boxed $V is not healthy: rolling back" >&2
	journalctl -u boxed -n 20 --no-pager >&2
	if [ -n "$prev" ]; then switch "$prev"; restart; fi
	exit 1
fi
# Keep the last five versions.
ls -1t $LIB/boxed-* | tail -n +6 | xargs -r rm
echo "boxed $V running"
SH
