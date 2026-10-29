#!/bin/bash
# microVM network on exe-host: bridge br0 10.42.0.1/16, NAT to the outside,
# VMs isolated from each other, from exe-host and from private networks.
# Idempotent: can run again while VMs are up (boxd's per-VM entries stay).
# Every rule set is replaced atomically, so isolation never lapses during a
# reload, and only chains of ours are rewritten.
set -euo pipefail
if [ -r /etc/exe/net.conf ]; then . /etc/exe/net.conf; fi
SUBNET=${BOX_SUBNET:-10.42.0.0/16}
GATEWAY=${BOX_GATEWAY:-10.42.0.1/16}
PROXY=${BOX_PROXY:-}   # the only remote client of boxd's web port (the TLS proxy); empty: local only
WEB_PORT=${BOX_WEB_PORT:-8080}
PRIVATE=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,169.254.0.0/16

ip link show br0 >/dev/null 2>&1 || ip link add br0 type bridge
ip addr replace "$GATEWAY" dev br0
ip link set br0 up
sysctl -qw net.ipv4.ip_forward=1

# Anti-spoofing on the bridge ports: a tap only sends frames with its own MAC
# and IPv4 address, nothing but IPv4 and ARP. boxd fills the sets per VM; a tap
# not in them sends nothing. One nft transaction: atomic.
nft -f - <<'NFT'
table bridge box {
	set mac { type ifname . ether_addr; }
	set ip4 { type ifname . ipv4_addr; }
	chain guard { type filter hook prerouting priority 0; policy accept; }
}
flush chain bridge box guard
table bridge box {
	chain guard {
		iifname != "tap*" accept
		iifname . ether saddr != @mac drop
		ether type arp iifname . arp saddr ether != @mac drop
		ether type arp iifname . arp saddr ip != @ip4 drop
		ether type ip iifname . ip saddr != @ip4 drop
		ether type != { ip, arp } drop
	}
}
NFT

proxy_rule=""
[ -n "$PROXY" ] && proxy_rule="-A BOX-INPUT -p tcp --dport $WEB_PORT -s $PROXY -j ACCEPT"

# Our chains, each replaced as a whole in one transaction per table
# (--noflush keeps everybody else's rules).
iptables-restore --noflush <<EOF
*nat
:BOX-NAT - [0:0]
-A BOX-NAT -s $SUBNET ! -o br0 -j MASQUERADE
COMMIT
*mangle
:BOX-FORWARD - [0:0]
# What VMs may not reach: each other, the LAN, the tailnet. In mangle, which
# Docker and Tailscale leave alone: Tailscale's ts-forward accepts everything
# going out on tailscale0 before any filter rule of ours.
-A BOX-FORWARD -i br0 -o br0 -j DROP
-A BOX-FORWARD -i br0 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
-A BOX-FORWARD -i br0 -d $PRIVATE -j DROP
# Nothing reaches a VM from outside but answers: SSH and HTTP to VMs go
# through boxd, which connects from exe-host itself.
-A BOX-FORWARD -o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
-A BOX-FORWARD -o br0 -j DROP
COMMIT
*filter
:BOX-DOCKER - [0:0]
:BOX-INPUT - [0:0]
# Docker sets FORWARD to DROP: VMs going out are accepted from DOCKER-USER.
-A BOX-DOCKER -i br0 -j ACCEPT
-A BOX-DOCKER -o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
# exe-host itself: VMs only get answers to what exe-host opened, and boxd's
# web port only takes connections from the proxy.
-A BOX-INPUT -i br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A BOX-INPUT -i br0 -j DROP
-A BOX-INPUT -p tcp --dport $WEB_PORT -i lo -j ACCEPT
$proxy_rule
-A BOX-INPUT -p tcp --dport $WEB_PORT -j DROP
COMMIT
EOF

# The same for IPv6: VMs have none, but boxd's web port must not be open there.
ip6tables-restore --noflush <<EOF
*filter
:BOX-INPUT - [0:0]
-A BOX-INPUT -p tcp --dport $WEB_PORT -i lo -j ACCEPT
-A BOX-INPUT -p tcp --dport $WEB_PORT -j DROP
COMMIT
EOF

# Hook our chains (once).
hook() { local cmd=$1 t=$2 from=$3 to=$4; $cmd -t "$t" -C "$from" -j "$to" 2>/dev/null || $cmd -t "$t" -I "$from" -j "$to"; }
iptables -N DOCKER-USER 2>/dev/null || true
iptables -C FORWARD -j DOCKER-USER 2>/dev/null || iptables -I FORWARD -j DOCKER-USER
hook iptables nat POSTROUTING BOX-NAT
hook iptables mangle FORWARD BOX-FORWARD
hook iptables filter DOCKER-USER BOX-DOCKER
hook iptables filter INPUT BOX-INPUT
hook ip6tables filter INPUT BOX-INPUT

# Rules of earlier versions of this script, written straight into shared chains.
iptables -t nat -D POSTROUTING -s "$SUBNET" ! -o br0 -j MASQUERADE 2>/dev/null || true
for r in "-i br0 -j ACCEPT" "-o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "-i tailscale0 -o br0 -j ACCEPT"; do
	iptables -D DOCKER-USER $r 2>/dev/null || true
done
