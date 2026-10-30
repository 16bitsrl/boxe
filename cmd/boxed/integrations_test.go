package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// input is `ssh boxe <cmd>` as user with stdin, as in `echo ... | ssh boxe ...`.
func (c *cli) input(user, cmd, stdin string) (stdout, stderr string, ok bool) {
	c.t.Helper()
	s, err := c.conn[user].NewSession()
	if err != nil {
		c.t.Fatal(err)
	}
	defer s.Close()
	var o, e bytes.Buffer
	s.Stdout, s.Stderr, s.Stdin = &o, &e, strings.NewReader(stdin)
	err = s.Run(cmd)
	return o.String(), e.String(), err == nil
}

func TestIntegrationsCommands(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test")
	c.must("alice", "new other")
	c.must("bob", "new bobs")
	out := c.must("alice", "integrations add http stripe --target https://api.stripe.com --header 'Authorization: Bearer sk_secret'")
	if !strings.Contains(out, "http://stripe.int.boxe/") || strings.Contains(out, "sk_secret") {
		t.Errorf("add:\n%s", out)
	}
	c.must("alice", "integrations add github gh --token ghp_secret --read-only")
	c.must("alice", "integrations attach stripe test")
	c.must("alice", "integrations attach stripe vm:other") // as exe.dev writes it
	c.must("alice", "integrations attach gh test")

	out = c.must("alice", "integrations")
	for _, want := range []string{"stripe", "https://api.stripe.com", "Authorization", "other, test", "gh", "github", "read-only"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk_secret") || strings.Contains(out, "ghp_secret") {
		t.Errorf("list shows a secret:\n%s", out)
	}
	var list []integrationJSON
	c.json("alice", "integrations --json", &list)
	if len(list) != 2 || list[0].Name != "gh" || !list[0].ReadOnly || list[1].Name != "stripe" || list[1].Header != "Authorization" ||
		strings.Join(list[1].VMs, ",") != "other,test" {
		t.Errorf("--json: %+v", list)
	}
	if out := c.must("alice", "integrations ls --json"); strings.Contains(out, "secret") {
		t.Errorf("--json shows a secret: %s", out)
	}
	// Bob sees none of Alice's.
	if out := c.must("bob", "integrations"); strings.Contains(out, "stripe") {
		t.Errorf("bob sees alice's integrations:\n%s", out)
	}

	for _, bad := range []struct{ user, cmd, want string }{
		{"alice", "integrations add http stripe --target https://x.example", "already"},
		{"alice", "integrations add http github --target https://x.example", "github"},
		{"alice", "integrations add http xx --target ftp://x.example", "target"},
		{"alice", "integrations add http xx --target https://u:p@x.example", "target"},
		{"alice", "integrations add http xx --target https://x.example --header nocolon", "header"},
		{"alice", "integrations add http xx --target https://x.example --header 'Host: y'", "header"},
		{"alice", "integrations add http X_Y --target https://x.example", "invalid name"},
		{"alice", "integrations add github xx", "--token"},
		{"alice", "integrations add smtp x", "http or github"},
		{"alice", "integrations attach nope test", "no integration named nope"},
		{"alice", "integrations attach stripe bobs", "no VM named bobs"},
		{"bob", "integrations attach stripe bobs", "no integration named stripe"},
		{"alice", "integrations detach stripe nope", "no VM named nope"},
	} {
		if out, errOut, ok := c.run(bad.user, bad.cmd); ok || !strings.Contains(errOut, bad.want) {
			t.Errorf("%s: ok %v, %s%s; want an error with %q", bad.cmd, ok, out, errOut, bad.want)
		}
	}
	// One GitHub token per VM.
	c.must("alice", "integrations add github gh2 --token ghp_other")
	if _, errOut, ok := c.run("alice", "integrations attach gh2 test"); ok || !strings.Contains(errOut, "gh") {
		t.Errorf("a second GitHub integration on test: %v %s", ok, errOut)
	}
	c.must("alice", "integrations attach gh2 other")

	c.must("alice", "integrations detach stripe other")
	c.json("alice", "integrations --json", &list)
	if got := strings.Join(list[2].VMs, ","); list[2].Name != "stripe" || got != "test" {
		t.Errorf("after detach: %+v", list)
	}
	c.must("alice", "integrations rm stripe")
	if _, err := c.box.store.Attached(VM{Name: "test", Owner: "alice"}, "stripe"); err != errNotFound {
		t.Errorf("rm left its attachment: %v", err)
	}
	if _, errOut, ok := c.run("alice", "integrations rm stripe"); ok || !strings.Contains(errOut, "no integration named stripe") {
		t.Errorf("rm twice: %v %s", ok, errOut)
	}
	if !strings.Contains(c.must("alice", "help"), "integrations attach") {
		t.Error("help does not show integrations")
	}
}

// A secret typed on the command line stays in the shell's history: "-"
// reads it from stdin instead.
func TestIntegrationSecretFromStdin(t *testing.T) {
	c := newCLI(t, "alice")
	if out, errOut, ok := c.input("alice", "integrations add http slack --target -", "https://hooks.slack.com/services/T0/B0/xyzsecret\n"); !ok {
		t.Fatalf("%s%s", out, errOut)
	}
	if out, errOut, ok := c.input("alice", "integrations add http api --target https://api.example.com --header -", "X-Api-Key: k3y\n"); !ok {
		t.Fatalf("%s%s", out, errOut)
	}
	if out, errOut, ok := c.input("alice", "integrations add github gh --token -", "ghp_fromstdin\n"); !ok {
		t.Fatalf("%s%s", out, errOut)
	}
	// The webhook's URL is the secret: its path is not shown.
	out := c.must("alice", "integrations")
	if strings.Contains(out, "xyzsecret") || !strings.Contains(out, "https://hooks.slack.com/…") || strings.Contains(out, "k3y") {
		t.Errorf("list:\n%s", out)
	}
	ig, err := c.box.store.Integration("alice", "api")
	if err != nil || ig.HeaderName != "X-Api-Key" || ig.Secret != "k3y" {
		t.Errorf("api: %+v %v", ig, err)
	}
	ig, _ = c.box.store.Integration("alice", "slack")
	if ig.Target != "https://hooks.slack.com/services/T0/B0/xyzsecret" || ig.HeaderName != "" {
		t.Errorf("slack: %+v", ig)
	}
	ig, _ = c.box.store.Integration("alice", "gh")
	if ig.Secret != "ghp_fromstdin" {
		t.Errorf("gh: %+v", ig)
	}
	if _, errOut, ok := c.input("alice", "integrations add http empty --target https://x.example --header -", ""); ok || !strings.Contains(errOut, "header") {
		t.Errorf("empty stdin: %v %s", ok, errOut)
	}
}

// fromVM is a listener whose connections come from a VM's address, as the
// proxy sees them on the bridge.
type fromVM struct {
	net.Listener
	mu sync.Mutex
	ip string
}

func (l *fromVM) set(ip string) { l.mu.Lock(); l.ip = ip; l.mu.Unlock() }

func (l *fromVM) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return vmConn{c, &net.TCPAddr{IP: net.ParseIP(l.ip), Port: 40000}}, nil
}

type vmConn struct {
	net.Conn
	addr net.Addr
}

func (c vmConn) RemoteAddr() net.Addr { return c.addr }

// serveProxy runs the integrations proxy as VMs reach it; the client sends
// every request there, whatever the host.
func serveProxy(t *testing.T, p *Proxy) (*fromVM, *http.Client) {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	l := &fromVM{Listener: ln, ip: "10.42.0.2"}
	go p.Serve(l)
	t.Cleanup(func() { ln.Close() })
	return l, &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func testProxy(c *cli) *Proxy {
	p := c.box.proxy()
	p.transport = &http.Transport{} // the real one refuses local addresses
	return p
}

func TestProxyAddsTheHeader(t *testing.T) {
	var got *http.Request
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Location", "https://elsewhere.example/next")
		w.WriteHeader(http.StatusFound)
		io.WriteString(w, "moved")
	}))
	defer up.Close()
	c := newCLI(t, "alice")
	c.must("alice", "new test") // 10.42.0.2
	c.must("alice", "integrations add http stripe --target "+up.URL+"/base --header 'Authorization: Bearer sk_secret'")
	c.must("alice", "integrations attach stripe test")
	_, client := serveProxy(t, testProxy(c))

	req, _ := http.NewRequest("POST", "http://stripe.int.boxe/v1/customers?limit=3", strings.NewReader("name=Jenny"))
	req.Header.Set("Authorization", "Bearer forged")
	req.Header.Set("X-Custom", "kept")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "dropped")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://elsewhere.example/next" || string(b) != "moved" {
		t.Errorf("answer: %d %v %q", res.StatusCode, res.Header, b)
	}
	if got == nil {
		t.Fatal("nothing reached the target")
	}
	if got.URL.Path != "/base/v1/customers" || got.URL.RawQuery != "limit=3" || body != "name=Jenny" || got.Method != "POST" {
		t.Errorf("target got %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, body)
	}
	if a := got.Header.Values("Authorization"); len(a) != 1 || a[0] != "Bearer sk_secret" {
		t.Errorf("Authorization: %q", a)
	}
	if got.Header.Get("X-Custom") != "kept" || got.Header.Get("X-Hop") != "" || got.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("headers: %v", got.Header)
	}
	if got.Host != strings.TrimPrefix(up.URL, "http://") {
		t.Errorf("Host %q", got.Host)
	}
}

// Without a header the target URL is the secret, e.g. a Slack webhook.
func TestProxyWithoutHeader(t *testing.T) {
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path }))
	defer up.Close()
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.input("alice", "integrations add http slack --target -", up.URL+"/services/T0/B0/xyz\n")
	c.must("alice", "integrations attach slack test")
	_, client := serveProxy(t, testProxy(c))
	res, err := client.Post("http://slack.int.boxe/", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", res, err)
	}
	if path != "/services/T0/B0/xyz" {
		t.Errorf("path %q", path)
	}
}

// Only VMs the integration is attached to get through, and a refusal does
// not tell whether the integration exists.
func TestProxyOnlyAttached(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer up.Close()
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test")  // 10.42.0.2
	c.must("alice", "new other") // 10.42.0.3
	c.must("bob", "new bobs")    // 10.42.0.4
	c.must("alice", "integrations add http stripe --target "+up.URL+" --header 'X-Key: s3cret'")
	c.must("alice", "integrations attach stripe test")
	l, client := serveProxy(t, testProxy(c))
	status := func(ip, url string) (int, string) {
		l.set(ip)
		client.CloseIdleConnections()
		res, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
	if s, _ := status("10.42.0.2", "http://stripe.int.boxe/"); s != 200 {
		t.Errorf("attached VM: %d", s)
	}
	_, refused := status("10.42.0.3", "http://stripe.int.boxe/")
	for _, r := range []struct{ ip, url string }{
		{"10.42.0.3", "http://stripe.int.boxe/"}, // not attached
		{"10.42.0.4", "http://stripe.int.boxe/"}, // someone else's VM
		{"10.42.0.2", "http://nope.int.boxe/"},   // no such integration
		{"10.42.0.9", "http://stripe.int.boxe/"}, // no such VM
		{"10.42.0.2", "http://stripe.example/"},  // not an integration name
	} {
		if s, b := status(r.ip, r.url); s != http.StatusForbidden || b != refused || strings.Contains(b, "s3cret") {
			t.Errorf("%s %s: %d %q", r.ip, r.url, s, b)
		}
	}
	if hits != 1 {
		t.Errorf("the target got %d requests, want 1", hits)
	}
}

// A connection a VM opened is not served for another VM that took its
// address later.
func TestProxyConnectionOfAReplacedVM(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "integrations add http stripe --target "+up.URL)
	c.must("alice", "integrations attach stripe test")
	_, client := serveProxy(t, testProxy(c))
	get := func() int {
		res, err := client.Get("http://stripe.int.boxe/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if s := get(); s != 200 {
		t.Fatalf("%d", s)
	}
	c.must("alice", "rm test")
	c.must("alice", "new again") // takes 10.42.0.2
	c.must("alice", "integrations attach stripe again")
	if s := get(); s != http.StatusForbidden {
		t.Errorf("old connection: %d", s)
	}
	client.CloseIdleConnections()
	if s := get(); s != 200 {
		t.Errorf("new connection: %d", s)
	}
}

// The real transport does not take VMs to the host or private networks
// through an integration.
func TestProxyRefusesLocalTargets(t *testing.T) {
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer up.Close()
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "integrations add http local --target "+up.URL+" --header 'X-Key: s3cret'")
	c.must("alice", "integrations attach local test")
	_, client := serveProxy(t, c.box.proxy())
	res, err := client.Get("http://local.int.boxe/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadGateway || hit || strings.Contains(string(b), "127.0.0.1") {
		t.Errorf("%d %q, reached: %v", res.StatusCode, b, hit)
	}
}

func TestCopiesKeepIntegrationsAndEgress(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "integrations add http stripe --target https://api.stripe.com")
	c.must("alice", "integrations attach stripe test")
	c.must("alice", "egress test integrations")
	c.must("alice", "cp test copy")
	c.must("alice", "fork test forked")
	for _, vm := range []string{"copy", "forked"} {
		v, _ := c.box.store.Get(vm)
		if _, err := c.box.store.Attached(v, "stripe"); err != nil {
			t.Errorf("%s: not attached: %v", vm, err)
		}
		if v.Egress != egressIntegrations {
			t.Errorf("%s: egress %q", vm, v.Egress)
		}
		if !c.host.ran(`nft add element bridge boxe closed { "` + tap(v) + `" }`) {
			t.Errorf("%s: its tap was not closed", vm)
		}
	}
	// A new VM with a deleted one's name gets nothing of it.
	c.must("alice", "rm copy")
	c.must("alice", "new copy")
	v, _ := c.box.store.Get("copy")
	if _, err := c.box.store.Attached(v, "stripe"); err != errNotFound {
		t.Errorf("new copy inherited the attachment: %v", err)
	}
	if v.Egress != egressAll {
		t.Errorf("new copy: egress %q", v.Egress)
	}
}

func TestEgressCommand(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test")
	out := c.must("alice", "egress test integrations")
	if !strings.Contains(out, "integrations") {
		t.Errorf("egress: %s", out)
	}
	if !c.host.ran(`nft add element bridge boxe closed { "tap2" }`) {
		t.Error("the tap was not closed")
	}
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	if list[0].Egress != "integrations" {
		t.Errorf("ls --json: %+v", list[0])
	}
	if out := c.must("alice", "ls"); !strings.Contains(out, "EGRESS") || !strings.Contains(out, "integrations") {
		t.Errorf("ls:\n%s", out)
	}
	// Applied again whenever the VM starts: after a restart, a wake, a reboot.
	c.host.cmds = nil
	c.must("alice", "restart test")
	if !c.host.ran(`nft add element bridge boxe closed { "tap2" }`) {
		t.Error("not closed again on restart")
	}
	c.must("alice", "sleep test")
	c.host.cmds = nil
	c.must("alice", "wake test")
	if !c.host.ran(`nft add element bridge boxe closed { "tap2" }`) {
		t.Error("not closed again on wake")
	}
	var j vmJSON
	c.json("alice", "egress test all --json", &j)
	if j.Egress != "all" || !c.host.ran(`nft delete element bridge boxe closed { "tap2" }`) {
		t.Errorf("egress all: %+v", j)
	}
	for _, bad := range []struct{ user, cmd string }{
		{"alice", "egress test none"},
		{"alice", "egress test"},
		{"bob", "egress test integrations"},
	} {
		if _, _, ok := c.run(bad.user, bad.cmd); ok {
			t.Errorf("%s: succeeded", bad.cmd)
		}
	}
	c.must("alice", "egress test integrations")
	c.host.cmds = nil
	c.must("alice", "rm test")
	if !c.host.ran(`nft delete element bridge boxe closed { "tap2" }`) {
		t.Error("rm left the tap closed")
	}
}

// git over smart HTTP, against git http-backend standing in for github.com.
func TestGitHubIntegration(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	env := append(os.Environ(), "HOME="+root, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	bare := filepath.Join(root, "repos", "owner", "repo.git")
	must(root, "init", "-q", "--bare", "-b", "main", bare)
	must(bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	must(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644)
	must(seed, "add", ".")
	must(seed, "commit", "-qm", "first")
	must(seed, "push", "-q", bare, "main")

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_secret"))
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "repos"), "GIT_HTTP_EXPORT_ALL=1"}}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer gh.Close()
	var apiAuth, apiPath string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth, apiPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, `{"login":"alice"}`)
	}))
	defer api.Close()

	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "integrations add github work --token ghp_secret")
	c.must("alice", "integrations attach work test")
	p := testProxy(c)
	p.githubURL, p.githubAPI = gh.URL, api.URL
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go p.Serve(&fromVM{Listener: ln, ip: "10.42.0.2"})
	defer ln.Close()
	// git sends everything to the proxy, as if github.int.boxe resolved to it.
	viaProxy := []string{"-c", "http.proxy=http://" + ln.Addr().String()}

	clone := filepath.Join(root, "clone")
	must(root, append(viaProxy, "clone", "-q", "http://github.int.boxe/owner/repo.git", clone)...)
	os.WriteFile(filepath.Join(clone, "NEW"), []byte("x\n"), 0o644)
	must(clone, "add", ".")
	must(clone, "commit", "-qm", "second")
	must(clone, append(viaProxy, "push", "-q", "origin", "main")...)
	if out, _ := git(bare, "log", "--oneline"); !strings.Contains(out, "second") {
		t.Errorf("push did not arrive: %s", out)
	}
	// Under its own name too.
	must(root, append(viaProxy, "ls-remote", "http://work.int.boxe/owner/repo.git")...)

	_, client := serveProxy(t, p)
	res, err := client.Get("http://github.int.boxe/api/user")
	if err != nil || res.StatusCode != 200 || apiAuth != "Bearer ghp_secret" || apiPath != "/user" {
		t.Errorf("api: %v %v, %q %q", res, err, apiAuth, apiPath)
	}

	// Read-only: fetch, but no push and no API call that changes anything.
	c.must("alice", "integrations detach work test")
	c.must("alice", "integrations add github ro --token ghp_secret --read-only")
	c.must("alice", "integrations attach ro test")
	must(clone, append(viaProxy, "fetch", "-q", "origin")...)
	os.WriteFile(filepath.Join(clone, "THIRD"), []byte("x\n"), 0o644)
	must(clone, "add", ".")
	must(clone, "commit", "-qm", "third")
	if out, err := git(clone, append(viaProxy, "push", "-q", "origin", "main")...); err == nil {
		t.Errorf("push through a read-only integration: %s", out)
	}
	if out, _ := git(bare, "log", "--oneline"); strings.Contains(out, "third") {
		t.Error("read-only push arrived")
	}
	for _, r := range []struct{ method, url string }{
		{"POST", "http://github.int.boxe/api/repos/owner/repo/issues"},
		{"DELETE", "http://github.int.boxe/api/repos/owner/repo"},
		{"POST", "http://github.int.boxe/owner/repo.git/git-receive-pack"},
		{"GET", "http://github.int.boxe/owner/repo.git/info/refs?service=git-receive-pack"},
	} {
		req, _ := http.NewRequest(r.method, r.url, nil)
		res, err := client.Do(req)
		if err != nil || res.StatusCode != http.StatusForbidden {
			t.Errorf("read-only %s %s: %v %v", r.method, r.url, res, err)
		}
	}
	if res, err := client.Get("http://github.int.boxe/api/user"); err != nil || res.StatusCode != 200 {
		t.Errorf("read-only GET: %v %v", res, err)
	}
	// Only git and the API: not github.com's pages with the token.
	if res, err := client.Get("http://github.int.boxe/settings/tokens"); err != nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("github.com page: %v %v", res, err)
	}
}

// Integrations reach public addresses only, and none of the host's own: a
// host with a public address would otherwise be reachable from its VMs.
func TestPublicAddresses(t *testing.T) {
	own := netip.MustParseAddr("203.0.113.7")
	for ip, want := range map[string]bool{
		"93.184.215.14": true, "2606:4700::1111": true,
		"203.0.113.7": false, "::ffff:203.0.113.7": false,
		"10.42.0.1": false, "192.168.1.1": false, "100.64.0.7": false, "127.0.0.1": false,
		"169.254.169.254": false, "::1": false, "fd00::1": false, "0.0.0.0": false,
	} {
		if got := public(netip.MustParseAddr(ip), []netip.Addr{own}); got != want {
			t.Errorf("%s: %v, want %v", ip, got, want)
		}
	}
}

// When the target's URL is the secret, a redirect or a header naming it
// does not hand it to the VM: it is told the integration's own address.
func TestProxyHidesTheTargetInAnswers(t *testing.T) {
	var host string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://"+host+"/services/T0/B0/xyz/")
		w.Header().Set("Content-Location", "/services/T0/B0/xyz")
		w.Header().Set("Link", "<http://"+host+"/services/T0/B0/xyz/more>; rel=next")
		w.Header().Set("Refresh", "0; url=/services/T0/B0/xyz/again")
		w.Header().Set("Set-Cookie", "s=1; Path=/services/T0/B0/xyz")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer up.Close()
	host = strings.TrimPrefix(up.URL, "http://")
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.input("alice", "integrations add http slack --target -", up.URL+"/services/T0/B0/xyz\n")
	c.must("alice", "integrations attach slack test")
	_, client := serveProxy(t, testProxy(c))
	res, err := client.Post("http://slack.int.boxe/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for k, v := range res.Header {
		if strings.Contains(strings.Join(v, " "), "xyz") {
			t.Errorf("%s gives the secret away: %q", k, v)
		}
	}
	if l := res.Header.Get("Location"); l != "http://slack.int.boxe/" {
		t.Errorf("Location %q", l)
	}
	if l := res.Header.Get("Link"); l != "<http://slack.int.boxe/more>; rel=next" {
		t.Errorf("Link %q", l)
	}
}

// Redirects to other hosts are left alone, even with a path like the target's.
func TestHideTargetLeavesOtherHosts(t *testing.T) {
	u, _ := url.Parse("https://hooks.example/services/x")
	h := http.Header{"Location": {"https://other.example/services/x/y"}}
	hideTarget(h, u, "http://hook.int.boxe")
	if l := h.Get("Location"); l != "https://other.example/services/x/y" {
		t.Errorf("Location %q", l)
	}
}
