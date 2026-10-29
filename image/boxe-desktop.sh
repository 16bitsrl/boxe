#!/bin/sh
# The desktop: Xvnc, an X server that is also a VNC server (localhost only,
# no password: only noVNC behind boxed reaches it), and a window manager.
Xvnc :1 -geometry 1600x1000 -depth 24 -SecurityTypes None -localhost -rfbport 5901 \
	-AlwaysShared -AcceptSetDesktopSize &
until [ -S /tmp/.X11-unix/X1 ]; do sleep 0.1; done
exec openbox-session
