package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// split is boxed -front and boxed in one test: users talk to the front,
// and the backend can be stopped and started again as a deploy does.
type split struct {
	t         *testing.T
	dir       string
	store     *Store
	keys      *Keys
	hv        *Hypervisor
	box       *Box
	web       *Web
	idp       *fakeIdP
	be        *backend
	front     *Front
	sshAddr   string // the front's SSH server
	webAddr   string // the front's HTTP server
	vmWebAddr string // the VM's web server
	signer    gossh.Signer

	mu   sync.Mutex
	seen []*http.Request // requests that reached the VM's web server
}

func newSplit(t *testing.T) *split {
	t.Helper()
	// Unix socket paths are short (104 bytes on macOS): not t.TempDir().
	dir, err := os.MkdirTemp("", "bx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h, f := newFakeHypervisor(t)
	h.SSHSuffix, h.WebDomain = "boxe", "boxe.example"
	s := &split{t: t, dir: dir, hv: h, idp: newFakeIdP(t)}
	// The VM: sshd on port 22 (a banner, then an echo), a web server on the others.
	sshd := listenTCP(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-fake\r\n"))
		io.Copy(c, c)
	})
	web := &http.Server{Handler: http.HandlerFunc(s.vmWeb)}
	webLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go web.Serve(webLn)
	s.vmWebAddr = webLn.Addr().String()
	t.Cleanup(func() { web.Close() })
	h.Dial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		if network == "unix" {
			return f.dial(network, addr, timeout)
		}
		if strings.HasSuffix(addr, ":22") {
			return net.DialTimeout("tcp", sshd, timeout)
		}
		return net.DialTimeout("tcp", webLn.Addr().String(), timeout)
	}
	s.store, err = openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	s.signer, _ = gossh.NewSignerFromKey(sk)
	s.keys = &Keys{store: s.store}
	s.keys.set(map[string]string{string(s.signer.PublicKey().Marshal()): "alice"}, map[string]string{})
	s.startBackend()
	t.Cleanup(func() { s.be.Close() })

	s.front = newFront(dir)
	s.front.wait = 2 * time.Second
	hostKey, _ := loadHostKey(filepath.Join(t.TempDir(), "host"))
	srv := s.front.sshServer(hostKey)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	s.sshAddr = ln.Addr().String()
	hs := &http.Server{Handler: s.front}
	hln, _ := net.Listen("tcp", "127.0.0.1:0")
	go hs.Serve(hln)
	t.Cleanup(func() { hs.Close() })
	s.webAddr = hln.Addr().String()
	ctl, err := listenUnix(filepath.Join(dir, frontSock))
	if err != nil {
		t.Fatal(err)
	}
	cs := &http.Server{Handler: s.front.control()}
	go cs.Serve(ctl)
	t.Cleanup(func() { cs.Close() })
	return s
}

// startBackend starts boxed as a new process would: nothing in memory.
func (s *split) startBackend() {
	s.t.Helper()
	s.box = &Box{store: s.store, keys: s.keys, hv: s.hv, name: "boxe", front: &frontClient{sock: filepath.Join(s.dir, frontSock)}}
	s.web = NewWeb(WebConfig{Domain: "boxe.example", Issuer: s.idp.URL, ClientID: "client", ClientSecret: "secret",
		OrgID: testOrg, Secret: []byte("0123456789abcdef0123456789abcdef")}, s.store)
	s.web.vmLock = &s.box.mu
	s.web.busy = s.box.busy
	s.web.ownerEmail = func(string) string { return "alice@example.com" }
	s.web.vmAddr = func(VM, int) string { return s.vmWebAddr }
	var err error
	if s.be, err = serveBackend(s.box, s.web, s.dir); err != nil {
		s.t.Fatal(err)
	}
}

// stopBackend stops boxed, as a deploy does before starting the new one.
func (s *split) stopBackend() { s.be.Close() }

// vmWeb is the VM's web server: an echo after an upgrade, "hello" otherwise.
func (s *split) vmWeb(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r)
	s.mu.Unlock()
	if r.Header.Get("Upgrade") == "" {
		io.WriteString(w, "hello from "+r.Host)
		return
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	// A VM trying to make the front count its connection as another VM's.
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\nX-Box-Front-Vm: other\r\n\r\nwelcome\n")
	brw.Flush()
	io.Copy(c, brw)
}

func (s *split) lastSeen() *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[len(s.seen)-1]
}

func listenTCP(t *testing.T, serve func(net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); serve(c) }()
		}
	}()
	return ln.Addr().String()
}

func (s *split) dialSSH() *gossh.Client {
	s.t.Helper()
	c, err := gossh.Dial("tcp", s.sshAddr, &gossh.ClientConfig{User: "alice",
		Auth: []gossh.AuthMethod{gossh.PublicKeys(s.signer)}, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { c.Close() })
	return c
}

// run is `ssh boxe <cmd>` over c, through the front.
func run(t *testing.T, c *gossh.Client, cmd string) (string, error) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var o, e bytes.Buffer
	sess.Stdout, sess.Stderr = &o, &e
	err = sess.Run(cmd)
	return o.String() + e.String(), err
}

// echo writes line to c and checks it comes back.
func echo(t *testing.T, c net.Conn, r *bufio.Reader, line string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
	got, err := r.ReadString('\n')
	if err != nil || got != line+"\n" {
		t.Fatalf("echo of %q: %q, %v", line, got, err)
	}
}

// login gets alice a session on host, the way a browser does.
func (s *split) login(host string) []*http.Cookie {
	wt := &webTest{t: s.t, web: s.web, idp: s.idp}
	b := wt.browser("member")
	b.get("https://"+host+"/", nil)
	return b.jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: "/"})
}

func (s *split) frontConns() map[string]int {
	s.t.Helper()
	c, err := (&frontClient{sock: filepath.Join(s.dir, frontSock)}).conns()
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// Deploys restart boxed, but not the front: SSH connections into VMs,
// and the front's connection itself, go on.
func TestSplitJumpSurvivesBackendRestart(t *testing.T) {
	s := newSplit(t)
	c := s.dialSSH()
	if out, err := run(t, c, "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	conn, err := c.Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	if banner, _ := r.ReadString('\n'); !strings.HasPrefix(banner, "SSH-") {
		t.Fatalf("banner %q", banner)
	}
	echo(t, conn, r, "before")
	if n := s.frontConns()["test"]; n != 1 {
		t.Errorf("front counts %d connections to test, want 1", n)
	}

	s.stopBackend()
	echo(t, conn, r, "while boxed is down")
	if _, err := run(t, c, "ls"); err == nil {
		t.Error("ls worked without boxed")
	}
	s.startBackend()
	echo(t, conn, r, "after")
	if out, err := run(t, c, "ls"); err != nil || !strings.Contains(out, "test") {
		t.Errorf("ls after the restart: %v: %s", err, out)
	}
	// A new jump goes through the new boxed.
	conn2, err := c.Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatalf("jump after the restart: %v", err)
	}
	conn2.Close()
	if _, err := c.Dial("tcp", "nope.boxe:22"); err == nil {
		t.Error("jump to a VM that does not exist")
	}

	// The new boxed knows the VM is in use, from the front.
	s.box.sleepAfter = time.Nanosecond
	s.box.sleepIdle(time.Now())
	s.box.sleepIdle(time.Now().Add(time.Hour))
	if st := s.hv.State("test"); st != "running" {
		t.Fatalf("a VM with an open connection went %s", st)
	}
	conn.Close()
	for i := 0; s.frontConns()["test"] != 0; i++ {
		if i == 100 {
			t.Fatal("front still counts the closed connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.box.sleepIdle(time.Now().Add(2 * time.Hour))
	if st := s.hv.State("test"); st != "sleeping" {
		t.Errorf("unused VM is %s", st)
	}
}

// The interactive bits of a session go through the front: stdin, the
// exit status of a command run in a VM.
func TestSplitSessionProxy(t *testing.T) {
	s := newSplit(t)
	c := s.dialSSH()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sess.Stdin = strings.NewReader("Authorization: Bearer sk-test\n")
	out, err := sess.CombinedOutput("integrations add http stripe --target https://api.stripe.com --header -")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if ig, err := s.store.Integrations("alice"); err != nil || len(ig) != 1 || !strings.Contains(ig[0].Secret, "sk-test") {
		t.Errorf("stdin did not reach boxed: %+v %v", ig, err)
	}
	if _, err := run(t, c, "rm nope"); err == nil {
		t.Error("a failed command exited 0")
	} else if exit, ok := err.(*gossh.ExitError); !ok || exit.ExitStatus() != 1 {
		t.Errorf("exit: %v", err)
	}
}

// A WebSocket to the desktop is the front's: boxed checks it and opens it,
// then hands it over, and it outlives a restart of boxed.
func TestSplitWebSocketSurvivesBackendRestart(t *testing.T) {
	s := newSplit(t)
	if out, err := run(t, s.dialSSH(), "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	host := "test--desktop.boxe.example"
	cookies := s.login(host)
	c, err := net.Dial("tcp", s.webAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req, _ := http.NewRequest("GET", "http://"+host+"/websockify", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "echo")
	req.Header.Set("Origin", "https://"+host)
	req.Header.Set("X-Box-Email", "boss@example.com")
	req.Header.Set("X-Box-Front-Addr", "6.6.6.6:1")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	req.Write(c)
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, req)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	if res.Header.Get("X-Box-Front-Vm") != "" {
		t.Error("the client sees the front's header")
	}
	if line, _ := r.ReadString('\n'); line != "welcome\n" {
		t.Fatalf("first bytes from the VM: %q", line)
	}
	vmReq := s.lastSeen()
	if vmReq.Header.Get("X-Box-Email") != "alice@example.com" || vmReq.Header.Get("X-Box-Front-Addr") != "" ||
		strings.Contains(vmReq.Header.Get("X-Forwarded-For"), "6.6.6.6") || vmReq.Header.Get("Cookie") != "" {
		t.Errorf("the VM got %v", vmReq.Header)
	}
	echo(t, c, r, "before")
	if n := s.frontConns(); n["test"] != 1 || n["other"] != 0 {
		t.Errorf("front counts %v", n)
	}
	s.stopBackend()
	echo(t, c, r, "while boxed is down")
	s.startBackend()
	echo(t, c, r, "after")
}

// Without a session, a WebSocket is refused as it is without the front.
func TestSplitWebSocketChecks(t *testing.T) {
	s := newSplit(t)
	if out, err := run(t, s.dialSSH(), "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	host := "test--desktop.boxe.example"
	cookies := s.login(host)
	for _, origin := range []string{"https://other.boxe.example", ""} {
		req, _ := http.NewRequest("GET", "http://"+s.webAddr+"/websockify", nil)
		req.Host = host
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "echo")
		req.Header.Set("Origin", origin)
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("from %q: %d", origin, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", "http://"+s.webAddr+"/websockify", nil)
	req.Host = host
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "echo")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Errorf("without a session: %d", res.StatusCode)
	}
}

// Web requests made while boxed restarts wait for it instead of failing.
func TestSplitWebWaitsForBackend(t *testing.T) {
	s := newSplit(t)
	if out, err := run(t, s.dialSSH(), "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	cookies := s.login("test.boxe.example")
	get := func() (*http.Response, string) {
		req, _ := http.NewRequest("GET", "http://"+s.webAddr+"/", nil)
		req.Host = "test.boxe.example"
		req.Header.Set("X-Box-Front-Addr", "6.6.6.6:1")
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	if res, b := get(); res.StatusCode != 200 || b != "hello from test.boxe.example" {
		t.Fatalf("%d %q", res.StatusCode, b)
	}
	if xff := s.lastSeen().Header.Get("X-Forwarded-For"); xff != "127.0.0.1" {
		t.Errorf("X-Forwarded-For %q", xff)
	}
	s.stopBackend()
	started := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.startBackend()
		close(started)
	}()
	start := time.Now()
	if res, b := get(); res.StatusCode != 200 || b != "hello from test.boxe.example" {
		t.Errorf("during the restart: %d %q", res.StatusCode, b)
	}
	<-started
	if time.Since(start) < 200*time.Millisecond {
		t.Error("answered before boxed was back")
	}
}

// Removing someone's key closes their connections through the front too.
func TestSplitRevokedKeyIsDisconnected(t *testing.T) {
	s := newSplit(t)
	c := s.dialSSH()
	if out, err := run(t, c, "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	conn, err := c.Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	r.ReadString('\n')
	s.box.revalidate() // nothing changed: stays connected
	echo(t, conn, r, "still here")

	s.keys.set(map[string]string{}, nil)
	s.box.revalidate()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(conn, "gone?\n")
	if line, err := r.ReadString('\n'); err == nil {
		t.Errorf("jump still open after the key was revoked: %q", line)
	}
}

func TestFrontConnsJSON(t *testing.T) {
	f := newFront(t.TempDir())
	f.count("a", 1)
	f.count("a", 1)
	f.count("b", 1)
	f.count("b", -1)
	if got := f.openConns(); len(got) != 1 || got["a"] != 2 {
		t.Errorf("open %v", got)
	}
}

// A new boxed has the front check every connection at once: whoever left
// the team while boxed was down loses their connections then, not at the
// front's next periodic check.
func TestSplitRestartRevalidates(t *testing.T) {
	s := newSplit(t)
	c := s.dialSSH()
	if out, err := run(t, c, "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	conn, err := c.Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	r.ReadString('\n')
	s.stopBackend()
	s.keys.set(map[string]string{}, nil)
	s.startBackend()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(conn, "gone?\n")
	if line, err := r.ReadString('\n'); err == nil {
		t.Errorf("jump still open after a restart without the key: %q", line)
	}
}
