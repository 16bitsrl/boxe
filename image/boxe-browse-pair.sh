#!/bin/bash
# Pairs browse with the shared Chrome, once per VM: browse pair prints a
# one-time code, and the extension's setup page takes it from the link.
# Chrome installs the extension by policy, from the image, on its first
# start.
set -u
browse=/usr/local/bin/browse
ext=$(cat /usr/local/share/boxe/browse-extension/id) || exit 1
"$browse" pair --list 2>/dev/null | grep -q '^No paired browsers' || exit 0
for _ in $(seq 240); do
	[ -d "$HOME/.config/google-chrome/Default/Extensions/$ext" ] && break
	sleep 0.5
done
log=$(mktemp)
trap 'rm -f "$log"' EXIT
"$browse" pair --wait-seconds 120 >"$log" 2>&1 &
code=
for _ in $(seq 40); do
	code=$(grep -oE '\b[A-Z0-9]{4}-[A-Z0-9]{4}\b' "$log" | head -1)
	[ -n "$code" ] && break
	sleep 0.25
done
if [ -z "$code" ]; then
	echo "browse pair printed no code:" >&2
	cat "$log" >&2
	exit 1
fi
google-chrome "chrome-extension://$ext/welcome.html#code=$code"
wait
