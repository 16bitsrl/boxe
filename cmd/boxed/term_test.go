package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeTerm echoes what is typed, as a shell with echo on would.
type fakeTerm struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	mu     sync.Mutex
	resize []string
}

func newFakeTerm() *fakeTerm {
	r, w := io.Pipe()
	return &fakeTerm{r: r, w: w}
}

func (f *fakeTerm) Read(b []byte) (int, error)  { return f.r.Read(b) }
func (f *fakeTerm) Write(b []byte) (int, error) { return f.w.Write(b) }
func (f *fakeTerm) Close() error                { return f.w.Close() }
func (f *fakeTerm) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resize = append(f.resize, fmt.Sprintf("%dx%d", cols, rows))
	return nil
}

// https://<vm>--term.<domain> is a terminal in the VM: like the agent, for
// its owner and who they gave access to, and only from its own page.
func TestWebTerminal(t *testing.T) {
	wt := newWebTest(t)
	owners := map[string]string{"alice": "alice@example.com"}
	wt.web.ownerEmail = func(u string) string { return owners[u] }
	var mu sync.Mutex
	var opened []string
	term := newFakeTerm()
	wt.web.shell = func(_ context.Context, v VM, session string, cols, rows int) (vmTerm, error) {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, fmt.Sprintf("%s %s %dx%d", v.Name, session, cols, rows))
		return term, nil
	}
	b := wt.browser("member")
	res := b.get("https://test--term.boxe.example/", nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "term.js") || wt.seen != nil {
		t.Fatalf("owner: %d, reached the VM's web: %v\n%s", res.StatusCode, wt.seen != nil, body)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q", csp)
	}
	if res := b.get("https://test--term.boxe.example/term.js", nil); res.StatusCode != 200 {
		t.Errorf("term.js: %d", res.StatusCode)
	}

	srv := httptest.NewServer(wt.web)
	defer srv.Close()
	dial := func(path, origin string) (*websocket.Conn, *http.Response, error) {
		u, _ := url.Parse("https://test--term.boxe.example/")
		h := http.Header{"Origin": {origin}}
		for _, c := range b.jar.Cookies(u) {
			h.Add("Cookie", c.Name+"="+c.Value)
		}
		addr := strings.TrimPrefix(srv.URL, "http://")
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return websocket.Dial(ctx, "ws://test--term.boxe.example"+path, &websocket.DialOptions{HTTPClient: client, HTTPHeader: h})
	}

	for _, origin := range []string{"https://other.boxe.example", "https://evil.example", ""} {
		if _, res, err := dial("/ws", origin); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Errorf("from %q: %v", origin, err)
		}
	}
	if _, res, err := dial("/ws?s=a%20b", "https://test--term.boxe.example"); err == nil || res == nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad session name: %v", err)
	}

	c, _, err := dial("/ws?s=work&cols=120&rows=40", "https://test--term.boxe.example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	typ, got, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(got) != "ls\r" {
		t.Fatalf("echo: %v %q %v", typ, got, err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	// The shell ends: so does the connection, normally.
	time.Sleep(50 * time.Millisecond)
	term.Close()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("after the shell exits: %v", err)
	}
	mu.Lock()
	if strings.Join(opened, ",") != "test work 120x40" {
		t.Errorf("opened %v", opened)
	}
	mu.Unlock()
	term.mu.Lock()
	if strings.Join(term.resize, ",") != "100x30" {
		t.Errorf("resized %v", term.resize)
	}
	term.mu.Unlock()

	// Another member, without access.
	owners["alice"] = "boss@example.com"
	if res := b.get("https://test--term.boxe.example/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("another member: %d", res.StatusCode)
	}
	if _, res, err := dial("/ws", "https://test--term.boxe.example"); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("another member's WebSocket: %v", err)
	}
}

// Under boxed -front, the terminal's WebSocket goes through the front to
// boxed, and the front counts it as a connection to the VM. A new boxed
// process closes it (the page reconnects to the same session).
func TestSplitTerminal(t *testing.T) {
	s := newSplit(t)
	if out, err := run(t, s.dialSSH(), "new test"); err != nil {
		t.Fatalf("new: %v: %s", err, out)
	}
	term := newFakeTerm()
	s.web.shell = func(context.Context, VM, string, int, int) (vmTerm, error) { return term, nil }
	host := "test--term.boxe.example"
	h := http.Header{"Origin": {"https://" + host}}
	for _, c := range s.login(host) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", s.webAddr)
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, "ws://"+host+"/ws", &websocket.DialOptions{HTTPClient: client, HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v %v", err, res)
	}
	if res.Header.Get(frontVMHeader) != "" {
		t.Error("the client sees the front's header")
	}
	c.Write(ctx, websocket.MessageBinary, []byte("pwd\r"))
	if _, got, err := c.Read(ctx); err != nil || string(got) != "pwd\r" {
		t.Fatalf("echo: %q %v", got, err)
	}
	if n := s.frontConns(); n["test"] != 1 {
		t.Errorf("front counts %v", n)
	}
	term.Close()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("after the shell exits: %v", err)
	}
}

// The session starts from a login shell: what /etc/profile.d sets (the LLM
// gateway) is there, as over SSH.
func TestTermCommandIsALoginShell(t *testing.T) {
	if got := termCommand("work"); !strings.Contains(got, "bash -lc 'exec scroll work'") || !strings.Contains(got, "exec bash -l;") {
		t.Errorf("%q", got)
	}
}
