package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeUpstream answers every A query with 93.184.216.34, over UDP.
func fakeUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil || len(m.Questions) == 0 {
				continue
			}
			m.Header.Response = true
			m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body: &dnsmessage.AResource{A: [4]byte{93, 184, 216, 34}}}}
			out, _ := m.Pack()
			pc.WriteTo(out, addr)
		}
	}()
	return pc.LocalAddr().String()
}

// serveDNS runs boxed's DNS server on UDP and TCP; closed says whether
// queries come from a VM that reaches integrations only.
func serveDNS(t *testing.T, closed bool) (udp, tcp string) {
	t.Helper()
	d := &DNS{Suffix: "boxe", Upstream: []string{fakeUpstream(t)}, Closed: func(netip.Addr) bool { return closed }}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); ln.Close() })
	go d.ServeUDP(pc)
	go d.ServeTCP(ln)
	return pc.LocalAddr().String(), ln.Addr().String()
}

func resolver(network, addr string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
}

func TestDNS(t *testing.T) {
	ctx := context.Background()
	for _, closed := range []bool{false, true} {
		udp, tcp := serveDNS(t, closed)
		for _, r := range []struct{ network, addr string }{{"udp", udp}, {"tcp", tcp}} {
			res := resolver(r.network, r.addr)
			// Integrations are on the gateway, whatever the VM's egress.
			for _, name := range []string{"stripe.int.boxe", "GitHub.int.boxe."} {
				ips, err := res.LookupIP(ctx, "ip4", name)
				if err != nil || len(ips) != 1 || ips[0].String() != "10.42.0.1" {
					t.Errorf("closed %v, %s %s: %v %v", closed, r.network, name, ips, err)
				}
			}
			// No IPv6 address, but the name exists.
			if _, err := res.LookupIP(ctx, "ip6", "stripe.int.boxe"); err == nil || isNotFound(err) == false {
				t.Errorf("closed %v, %s AAAA: %v", closed, r.network, err)
			}
			ips, err := res.LookupIP(ctx, "ip4", "example.com")
			if closed {
				if err == nil || !isNotFound(err) {
					t.Errorf("closed, %s example.com: %v %v", r.network, ips, err)
				}
			} else if err != nil || len(ips) != 1 || ips[0].String() != "93.184.216.34" {
				t.Errorf("open, %s example.com: %v %v", r.network, ips, err)
			}
		}
	}
}

func isNotFound(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d) && d.IsNotFound
}

// A VM is closed if it reaches integrations only; an address that is no VM
// gets nothing either.
func TestDNSClosedVMs(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")  // 10.42.0.2
	c.must("alice", "new other") // 10.42.0.3
	c.must("alice", "egress other integrations")
	closed := c.box.dnsClosed
	for ip, want := range map[string]bool{"10.42.0.2": false, "10.42.0.3": true, "10.42.0.9": true, "192.168.1.1": true} {
		if got := closed(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: closed %v, want %v", ip, got, want)
		}
	}
}
