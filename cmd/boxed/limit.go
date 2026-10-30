package main

import (
	"net"
	"net/netip"
	"sync"
)

// limiter counts what VMs hold of something (queries under way, open
// connections): at most perIP each, total in all.
type limiter struct {
	perIP, total int
	mu           sync.Mutex
	n            map[netip.Addr]int
	all          int
}

func newLimiter(perIP, total int) *limiter {
	return &limiter{perIP: perIP, total: total, n: map[netip.Addr]int{}}
}

// take counts one more for ip, if there is room.
func (l *limiter) take(ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[ip] >= l.perIP || l.all >= l.total {
		return false
	}
	l.n[ip]++
	l.all++
	return true
}

func (l *limiter) done(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[ip]--; l.n[ip] <= 0 {
		delete(l.n, ip)
	}
	l.all--
}

// limitListener closes the connections beyond what l allows.
func limitListener(ln net.Listener, l *limiter) net.Listener {
	return &limitedListener{ln, l}
}

type limitedListener struct {
	net.Listener
	l *limiter
}

func (ll *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := ll.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := addrIP(c.RemoteAddr())
		if ll.l.take(ip) {
			return &limitedConn{Conn: c, done: func() { ll.l.done(ip) }}, nil
		}
		c.Close()
	}
}

type limitedConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *limitedConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}
