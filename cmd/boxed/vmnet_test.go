package main

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// The VMs' network is the one net-setup.sh gave br0: addresses, the
// gateway, the mask and how many VMs fit follow from it.
func TestOtherVMNetwork(t *testing.T) {
	defer setVMNet(netip.MustParsePrefix("10.42.0.1/16"))
	if err := setVMNet(netip.MustParsePrefix("192.168.200.1/24")); err != nil {
		t.Fatal(err)
	}
	if g := gatewayIP(); g != "192.168.200.1" {
		t.Errorf("gateway %s", g)
	}
	v := VM{Name: "x", Index: 17}
	if v.IP() != "192.168.200.17" {
		t.Errorf("IP %s", v.IP())
	}
	if i, ok := vmAt(netip.MustParseAddr("192.168.200.17")); !ok || i != 17 {
		t.Errorf("vmAt %d %v", i, ok)
	}
	for _, ip := range []string{"10.42.0.17", "192.168.201.17", "192.168.200.1", "192.168.200.255"} {
		if _, ok := vmAt(netip.MustParseAddr(ip)); ok {
			t.Errorf("%s is a VM", ip)
		}
	}
	if maxIndex() != 254 {
		t.Errorf("max index %d", maxIndex())
	}
	h := &Hypervisor{}
	if c := h.cmdline(v); !strings.Contains(c, "ip=192.168.200.17::192.168.200.1:255.255.255.0:x:") {
		t.Errorf("cmdline %s", c)
	}

	st, err := openStore(t.TempDir() + "/boxe.db")
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 254; i++ {
		if err := st.Insert(&VM{Name: fmt.Sprintf("v%d", i), Owner: "a", CPUs: 1, Mem: "1G"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Insert(&VM{Name: "full", Owner: "a", CPUs: 1, Mem: "1G"}); err == nil {
		t.Error("a VM beyond the network")
	}

	// Not a network boxe can use.
	for _, bad := range []string{"10.42.0.5/16", "10.42.0.1/25", "fd00::1/64", "10.0.0.1/7"} {
		if err := setVMNet(netip.MustParsePrefix(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
