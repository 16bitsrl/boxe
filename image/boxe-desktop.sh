#!/bin/sh
# The desktop: KasmVNC's Xvnc, an X server that also serves the desktop to
# the browser on localhost:2997 (no password, no TLS: only boxed reaches it,
# through boxe-desktop-web), and a window manager.
Xvnc :1 -interface 127.0.0.1 -websocketPort 2997 -rfbport -1 -disableBasicAuth -SecurityTypes None -sslOnly 0 \
	-httpd /usr/share/kasmvnc/www -geometry 1600x900 -depth 24 -FrameRate 60 -AcceptSetDesktopSize 1 &
until [ -S /tmp/.X11-unix/X1 ]; do sleep 0.1; done
exec openbox-session
