#!/bin/bash
# microVM network on the host: bridge br0 on BOXE_SUBNET (10.42.0.0/16 if not
# set in /etc/boxe/net.conf), the host on its first address, NAT to the outside,
# VMs isolated from each other, from the host and from private networks.
# Idempotent: can run again while VMs are up (boxed's per-VM entries stay).
# Every rule set is replaced atomically, so isolation never lapses during a
# reload, and only chains of ours are rewritten.
set -euo pipefail
if [ -r /etc/boxe/net.conf ]; then . /etc/boxe/net.conf; fi
# boxed reads it back from br0: VMs get their addresses in it, and boxed
# listens on the first one. An IPv4 network from /8 to /24.
SUBNET=${BOXE_SUBNET:-10.42.0.0/16}
NET=${SUBNET%/*} BITS=${SUBNET#*/}
if ! [[ $NET =~ ^([0-9]+\.){3}0$ && $BITS =~ ^[0-9]+$ ]] || ((BITS < 8 || BITS > 24)); then
	echo "BOXE_SUBNET=$SUBNET: an IPv4 network from /8 to /24, like 10.42.0.0/16" >&2
	exit 1
fi
GATEWAY=${NET%.0}.1/$BITS
GW=${GATEWAY%/*}   # boxed listens here for the VMs: integrations (80) and DNS (53)
PROXY=${BOXE_PROXY:-}   # the only remote client of boxed's web port (the TLS proxy); empty: local only
WEB_PORT=${BOXE_WEB_PORT:-8080}
LLM_PORT=${BOXE_LLM_PORT:-2998} # boxed's LLM gateway (its -llm-port)
PRIVATE=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,169.254.0.0/16

ip link show br0 >/dev/null 2>&1 || ip link add br0 type bridge
ip addr replace "$GATEWAY" dev br0
# Only that address: boxed takes the network from br0's.
for a in $(ip -4 -o addr show dev br0 | awk '{print $4}'); do
	[ "$a" = "$GATEWAY" ] || ip addr del "$a" dev br0
done
ip link set br0 up
sysctl -qw net.ipv4.ip_forward=1

# Anti-spoofing on the bridge ports: a tap only sends frames with its own MAC
# and IPv4 address, nothing but IPv4 and ARP. boxed fills the sets per VM; a tap
# not in them sends nothing. Taps in "closed" (ssh boxe egress <vm>
# integrations) reach the gateway only. One nft transaction: atomic.
nft -f - <<NFT
table bridge boxe {
	set mac { type ifname . ether_addr; }
	set ip4 { type ifname . ipv4_addr; }
	set closed { type ifname; }
	chain guard { type filter hook prerouting priority 0; policy accept; }
}
flush chain bridge boxe guard
table bridge boxe {
	chain guard {
		iifname != "tap*" accept
		iifname . ether saddr != @mac drop
		ether type arp iifname . arp saddr ether != @mac drop
		ether type arp iifname . arp saddr ip != @ip4 drop
		ether type ip iifname . ip saddr != @ip4 drop
		ether type != { ip, arp } drop
		iifname @closed ether type arp accept
		iifname @closed ip daddr $GW accept
		# DNS to any server goes to the gateway's (BOXE-DNS below).
		iifname @closed udp dport 53 accept
		iifname @closed tcp dport 53 accept
		iifname @closed drop
	}
}
NFT

proxy_rule=""
[ -n "$PROXY" ] && proxy_rule="-A BOXE-INPUT -p tcp --dport $WEB_PORT -s $PROXY -j ACCEPT"

# Our chains, each replaced as a whole in one transaction per table
# (--noflush keeps everybody else's rules).
iptables-restore --noflush <<EOF
*nat
:BOXE-NAT - [0:0]
:BOXE-DNS - [0:0]
-A BOXE-NAT -s $SUBNET ! -o br0 -j MASQUERADE
# Every DNS query of a VM goes to boxed, whatever server it asked: VMs made
# before boxed had a DNS server still have 1.1.1.1 in resolv.conf.
-A BOXE-DNS -i br0 -p udp --dport 53 -j DNAT --to-destination $GW:53
-A BOXE-DNS -i br0 -p tcp --dport 53 -j DNAT --to-destination $GW:53
COMMIT
*mangle
:BOXE-FORWARD - [0:0]
# What VMs may not reach: each other, the LAN, the tailnet. In mangle, which
# Docker and Tailscale leave alone: Tailscale's ts-forward accepts everything
# going out on tailscale0 before any filter rule of ours.
-A BOXE-FORWARD -i br0 -o br0 -j DROP
# DNS goes to boxed only (BOXE-DNS): none passes through.
-A BOXE-FORWARD -i br0 -p udp --dport 53 -j DROP
-A BOXE-FORWARD -i br0 -p tcp --dport 53 -j DROP
-A BOXE-FORWARD -i br0 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
-A BOXE-FORWARD -i br0 -d $PRIVATE -j DROP
# Nothing reaches a VM from outside but answers: SSH and HTTP to VMs go
# through boxed, which connects from the host itself.
-A BOXE-FORWARD -o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
-A BOXE-FORWARD -o br0 -j DROP
COMMIT
*filter
:BOXE-DOCKER - [0:0]
:BOXE-INPUT - [0:0]
# Docker sets FORWARD to DROP: VMs going out are accepted from DOCKER-USER.
-A BOXE-DOCKER -i br0 -j ACCEPT
-A BOXE-DOCKER -o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
# The host itself: VMs only get answers to what the host opened, boxed's DNS,
# integrations and LLM gateway on the gateway address, and boxed's web port
# only takes connections from the proxy.
-A BOXE-INPUT -i br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A BOXE-INPUT -i br0 -d $GW -p udp --dport 53 -j ACCEPT
-A BOXE-INPUT -i br0 -d $GW -p tcp --dport 53 -j ACCEPT
-A BOXE-INPUT -i br0 -d $GW -p tcp --dport 80 -j ACCEPT
-A BOXE-INPUT -i br0 -d $GW -p tcp --dport $LLM_PORT -j ACCEPT
-A BOXE-INPUT -i br0 -j DROP
-A BOXE-INPUT -p tcp --dport $WEB_PORT -i lo -j ACCEPT
$proxy_rule
-A BOXE-INPUT -p tcp --dport $WEB_PORT -j DROP
COMMIT
EOF

# The same for IPv6: VMs have none, but boxed's web port must not be open there.
ip6tables-restore --noflush <<EOF
*filter
:BOXE-INPUT - [0:0]
-A BOXE-INPUT -p tcp --dport $WEB_PORT -i lo -j ACCEPT
-A BOXE-INPUT -p tcp --dport $WEB_PORT -j DROP
COMMIT
EOF

# Hook our chains (once).
hook() { local cmd=$1 t=$2 from=$3 to=$4; $cmd -t "$t" -C "$from" -j "$to" 2>/dev/null || $cmd -t "$t" -I "$from" -j "$to"; }
iptables -N DOCKER-USER 2>/dev/null || true
iptables -C FORWARD -j DOCKER-USER 2>/dev/null || iptables -I FORWARD -j DOCKER-USER
hook iptables nat POSTROUTING BOXE-NAT
hook iptables nat PREROUTING BOXE-DNS
hook iptables mangle FORWARD BOXE-FORWARD
hook iptables filter DOCKER-USER BOXE-DOCKER
hook iptables filter INPUT BOXE-INPUT
hook ip6tables filter INPUT BOXE-INPUT

# Rules of earlier versions of this script, written straight into shared chains.
iptables -t nat -D POSTROUTING -s "$SUBNET" ! -o br0 -j MASQUERADE 2>/dev/null || true
for r in "-i br0 -j ACCEPT" "-o br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "-i tailscale0 -o br0 -j ACCEPT"; do
	iptables -D DOCKER-USER $r 2>/dev/null || true
done
