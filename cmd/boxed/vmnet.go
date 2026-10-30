package main

import (
	"fmt"
	"net"
	"net/netip"
)

// The VMs' network: the one scripts/net-setup.sh gives br0 (BOXE_SUBNET in
// /etc/boxe/net.conf), which boxed reads at start. The host is its first
// address; a VM's index in the database is its place in it, which fixes its
// address and MAC.
var vmNet = netip.MustParsePrefix("10.42.0.1/16") // the gateway, with the network's length

// setVMNet takes the network from br0's address p: an IPv4 network of 256 to
// 2^24 addresses, with the bridge on its first one.
func setVMNet(p netip.Prefix) error {
	if !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 24 || p.Addr() != p.Masked().Addr().Next() {
		return fmt.Errorf("br0 is %s: it must be the first address of an IPv4 network from /8 to /24, e.g. 10.42.0.1/16", p)
	}
	vmNet = p
	return nil
}

// vmNetFromBridge sets the network from br0, as net-setup.sh made it.
func vmNetFromBridge() error {
	br, err := net.InterfaceByName("br0")
	if err != nil {
		return fmt.Errorf("no br0 (boxe-net.service makes it): %v", err)
	}
	addrs, err := br.Addrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
			return setVMNet(p)
		}
	}
	return fmt.Errorf("br0 has no IPv4 address (boxe-net.service gives it one)")
}

// gatewayIP is the host on the VMs' network.
func gatewayIP() string { return vmNet.Addr().String() }

// maxIndex is the highest index a VM can have: the network's last address
// but its broadcast, and no more than 65534 VMs.
func maxIndex() int { return min(1<<(32-vmNet.Bits())-2, 65534) }

func vmAddr(index int) netip.Addr {
	a := vmNet.Masked().Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) + uint32(index)
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// vmAt is the index of the VM whose address is ip, if ip can be a VM's.
func vmAt(ip netip.Addr) (int, bool) {
	ip = ip.Unmap()
	if !ip.Is4() || !vmNet.Contains(ip) {
		return 0, false
	}
	a, b := ip.As4(), vmNet.Masked().Addr().As4()
	i := int(uint32(a[0])<<24|uint32(a[1])<<16|uint32(a[2])<<8|uint32(a[3])) - int(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]))
	return i, i >= 2 && i <= maxIndex()
}

// vmMask is the network's mask, e.g. 255.255.0.0, for the kernel's ip=.
func vmMask() string { return net.IP(net.CIDRMask(vmNet.Bits(), 32)).String() }
