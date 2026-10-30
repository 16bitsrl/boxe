package main

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

// A VM gets so many of the host's resources at a time, and all VMs
// together so many: one cannot starve boxed with queries or connections.
func TestLimiter(t *testing.T) {
	l := newLimiter(2, 3)
	a, b := netip.MustParseAddr("10.42.0.2"), netip.MustParseAddr("10.42.0.3")
	if !l.take(a) || !l.take(a) || l.take(a) {
		t.Fatal("per VM limit")
	}
	if !l.take(b) || l.take(b) {
		t.Fatal("total limit")
	}
	l.done(a)
	if !l.take(b) {
		t.Error("not given back")
	}
}

func TestLimitListener(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	ll := limitListener(ln, newLimiter(1, 10))
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ll.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c1, _ := net.Dial("tcp", ln.Addr().String())
	defer c1.Close()
	s1 := <-accepted
	c2, _ := net.Dial("tcp", ln.Addr().String())
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Error("a second connection from the same address was kept")
	}
	s1.Close()
	s1.Close() // twice: given back once
	c3, _ := net.Dial("tcp", ln.Addr().String())
	defer c3.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Error("no connection after the first closed")
	}
}
