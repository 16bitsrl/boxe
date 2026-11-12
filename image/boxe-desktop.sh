#!/bin/bash
# The desktop: Xvfb (DISPLAY=:1), PulseAudio for its sound, voxl streaming
# both to the browser on localhost:2997 (no password, no TLS: only boxed
# reaches it, through boxe-desktop-web, and lets in who may use the VM), and
# a window manager. When one of them stops, the desktop stops, and systemd
# starts it again.
set -u

wait_for() {
	for _ in $(seq 100); do
		[ -S "$1" ] && return
		sleep 0.1
	done
	echo "$1 did not appear" >&2
	exit 1
}

# The largest the desktop can grow: voxl sizes it to the viewer's window.
Xvfb :1 -screen 0 2560x1600x24 -shmem -nolisten tcp &
wait_for /tmp/.X11-unix/X1
pulseaudio -n -F /usr/local/lib/boxe/desktop.pa --daemonize=no --exit-idle-time=-1 &
wait_for "$XDG_RUNTIME_DIR/pulse/native"
# voxl answers only names under this installation's domain (the VM's is
# <vm>--desktop.<domain>), besides localhost.
domain=$(cat /etc/boxe/web-domain 2>/dev/null)
voxl --addr 127.0.0.1 --port 2997 --enable-basic-auth=false ${domain:+--allowed-hosts ".$domain"} \
	--enable-resize --resize-scale 1 &
openbox-session &
wait -n
exit 1
