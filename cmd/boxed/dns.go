package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNS is the VMs' resolver, on the gateway: net-setup.sh sends it every DNS
// packet from br0, whatever server the VM asked. It answers the names of
// integrations, <name>.int.<suffix>, with the gateway, and forwards the
// rest upstream, but not for VMs that reach integrations only: they get
// no other name, so DNS cannot carry their data out.
type DNS struct {
	Suffix   string
	Upstream []string                 // e.g. 1.1.1.1, or host:port
	Closed   func(ip netip.Addr) bool // the VM at ip may resolve only integrations
}

func (d *DNS) ServeUDP(pc net.PacketConn) error {
	for {
		buf := make([]byte, 4096)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		go func() {
			if out := d.answer(buf[:n], addrIP(addr), false); out != nil {
				pc.WriteTo(out, addr)
			}
		}()
	}
}

func (d *DNS) ServeTCP(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			for {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				q, err := readTCPMsg(c)
				if err != nil {
					return
				}
				out := d.answer(q, addrIP(c.RemoteAddr()), true)
				if out == nil || writeTCPMsg(c, out) != nil {
					return
				}
			}
		}()
	}
}

func addrIP(a net.Addr) netip.Addr {
	ap, _ := netip.ParseAddrPort(a.String())
	return ap.Addr().Unmap()
}

func readTCPMsg(r io.Reader) ([]byte, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeTCPMsg(w io.Writer, m []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(m))), m...))
	return err
}

// answer is the reply to the query q from ip; nil for what is no query.
func (d *DNS) answer(q []byte, ip netip.Addr, tcp bool) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil || h.Response {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		return d.reply(h, dnsmessage.Question{}, dnsmessage.RCodeFormatError, false)
	}
	name := strings.ToLower(qs[0].Name.String())
	if zone := ".int." + d.Suffix + "."; strings.HasSuffix(name, zone) && len(name) > len(zone) {
		return d.reply(h, qs[0], dnsmessage.RCodeSuccess, qs[0].Type == dnsmessage.TypeA)
	}
	if d.Closed(ip) {
		return d.reply(h, qs[0], dnsmessage.RCodeNameError, false)
	}
	if out, err := d.forward(q, tcp); err == nil {
		return out
	}
	return d.reply(h, qs[0], dnsmessage.RCodeServerFailure, false)
}

// reply answers q with rcode and, with gateway, the gateway's address.
func (d *DNS) reply(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode, gateway bool) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode, Authoritative: rcode != dnsmessage.RCodeServerFailure,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rcode})
	b.EnableCompression()
	b.StartQuestions()
	if q.Name.Length > 0 {
		b.Question(q)
	}
	b.StartAnswers()
	if gateway {
		b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
			dnsmessage.AResource{A: netip.MustParseAddr(gatewayIP).As4()})
	}
	out, _ := b.Finish()
	return out
}

// forward sends q to the upstream servers in turn, over UDP; over TCP
// when the answer does not fit and the VM asked over TCP (over UDP it asks
// again over TCP when it sees the answer truncated).
func (d *DNS) forward(q []byte, tcp bool) ([]byte, error) {
	err := errors.New("no upstream DNS server")
	for _, s := range d.Upstream {
		addr := s
		if _, _, e := net.SplitHostPort(s); e != nil {
			addr = net.JoinHostPort(s, "53")
		}
		var out []byte
		out, err = exchange("udp", addr, q)
		if err == nil && tcp && len(out) > 2 && out[2]&0x02 != 0 { // TC: truncated
			out, err = exchange("tcp", addr, q)
		}
		if err == nil {
			return out, nil
		}
	}
	return nil, err
}

func exchange(network, addr string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	var out []byte
	if network == "tcp" {
		if err := writeTCPMsg(c, q); err != nil {
			return nil, err
		}
		out, err = readTCPMsg(c)
	} else {
		if _, err := c.Write(q); err != nil {
			return nil, err
		}
		buf := make([]byte, 65535)
		var n int
		n, err = c.Read(buf)
		out = buf[:n]
	}
	if err != nil {
		return nil, err
	}
	if len(out) < 12 || out[0] != q[0] || out[1] != q[1] {
		return nil, errors.New("unexpected answer")
	}
	return out, nil
}
