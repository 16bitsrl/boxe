package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// panelTest is the panel on https://boxe.example with real VMs (on a fake
// host): alice has test and nap, bob has bobs-vm, mattia, the admin, mine.
type panelTest struct {
	*webTest
	c *cli
}

func newPanelTest(t *testing.T) *panelTest {
	c := newCLI(t, "alice", "bob", "mattia")
	c.box.hv.WebDomain = "boxe.example"
	c.must("alice", "new test")
	c.must("alice", "new nap")
	c.must("bob", "new bobs-vm")
	c.must("mattia", "new mine")
	c.box.keys.mu.Lock()
	c.box.keys.emails = map[string]string{"alice": "alice@example.com", "bob": "bob@example.com", "mattia": "mattia@example.com"}
	c.box.keys.admins = map[string]bool{"mattia": true}
	c.box.keys.byUser = map[string]string{"alice": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlice alice@laptop\n"}
	c.box.keys.mu.Unlock()
	wt := &webTest{t: t, idp: newFakeIdP(t)}
	wt.web = NewWeb(WebConfig{
		Domain:       "boxe.example",
		Issuer:       wt.idp.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		OrgID:        testOrg,
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
	}, c.box.store)
	c.box.panel(wt.web)
	return &panelTest{webTest: wt, c: c}
}

// page logs in as code (if needed) and returns the page at path of the panel.
func (b *browser) page(path string) (int, string) {
	res := b.get("https://boxe.example"+path, nil)
	return res.StatusCode, body(res)
}

// Opening the panel logs the member in at the IdP, then shows their VMs,
// and only theirs.
func TestPanelShowsMyVMs(t *testing.T) {
	pt := newPanelTest(t)
	code, page := pt.browser("member").page("/")
	if code != 200 || pt.idp.authorizes.Load() != 1 {
		t.Fatalf("%d, %d logins:\n%s", code, pt.idp.authorizes.Load(), page)
	}
	for _, want := range []string{">test<", ">nap<", "https://test.boxe.example", "ssh test.boxe", "running"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	for _, other := range []string{"bobs-vm", "mine"} {
		if strings.Contains(page, other) {
			t.Errorf("alice sees %s", other)
		}
	}
}

// After the login the member lands on the page they asked for.
func TestPanelLoginReturnsToThePage(t *testing.T) {
	pt := newPanelTest(t)
	res := pt.browser("member").get("https://boxe.example/vm/test", nil)
	if page := body(res); res.StatusCode != 200 || !strings.Contains(page, "ssh test.boxe") {
		t.Fatalf("%d:\n%s", res.StatusCode, page)
	}
	b := pt.browser("member")
	b.page("/")
	// Logged in, the login goes straight back: only to a page of the panel.
	res = b.do("https://boxe.example/__boxe/login?"+url.Values{"r": {"https://boxe.example//evil.example/x"}}.Encode(), nil)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		t.Errorf("//evil.example: %d %q", res.StatusCode, loc)
	}
	for _, bad := range []string{"https://boxe.example.evil/", "http://boxe.example/", "https://user@boxe.example/", "https://boxe.example:8443/"} {
		res := b.do("https://boxe.example/__boxe/login?"+url.Values{"r": {bad}}.Encode(), nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("r=%s: %d", bad, res.StatusCode)
		}
	}
}

// The detail of a VM: what it is, what it uses and how to reach it.
func TestPanelVMDetail(t *testing.T) {
	pt := newPanelTest(t)
	h := pt.c.host
	h.cpu[unit("test")] = 1e9
	pt.c.box.sleepIdle(time.Now().Add(-10 * time.Second))
	h.cpu[unit("test")] = 6e9 // 5 s of CPU in 10 s
	h.mem = map[string]int64{unit("test"): 700 << 20}
	h.disk = "boxe/vms/test\t5476083302\n"
	code, page := pt.browser("member").page("/vm/test")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{"ssh test.boxe", "https://test.boxe.example", "https://test--agent.boxe.example",
		"https://test--desktop.boxe.example", "https://test--3000.boxe.example", "50%", "700M", "5.1G", "10.42.0.2", "running"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	if code, page := pt.browser("member").page("/"); code != 200 || !strings.Contains(page, "5.1G") || !strings.Contains(page, "50%") {
		t.Errorf("the list lacks the usage: %d\n%s", code, page)
	}
}

// Someone else's VM does not exist for a member, as in ssh boxe.
func TestPanelHidesOthersVMs(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	for _, path := range []string{"/vm/bobs-vm", "/vm/nope", "/vm/", "/nope"} {
		if code, page := b.page(path); code != 404 || strings.Contains(page, "bob") {
			t.Errorf("%s: %d\n%s", path, code, page)
		}
	}
}

// The admin sees every VM, theirs first, with its owner, and the detail of
// any; the agent and the desktop stay the owner's.
func TestPanelAdminSeesAll(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("admin")
	code, page := b.page("/")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	mine, bobs, test := strings.Index(page, ">mine<"), strings.Index(page, ">bobs-vm<"), strings.Index(page, ">test<")
	if mine < 0 || bobs < 0 || test < 0 || mine > bobs || bobs > test || !strings.Contains(page, "alice") {
		t.Errorf("order mine %d, bobs-vm %d, test %d:\n%s", mine, bobs, test, page)
	}
	code, page = b.page("/vm/bobs-vm")
	if code != 200 || !strings.Contains(page, "bob") || !strings.Contains(page, "ssh bobs-vm.boxe") {
		t.Fatalf("%d:\n%s", code, page)
	}
	if strings.Contains(page, "bobs-vm--agent") || strings.Contains(page, "bobs-vm--desktop") {
		t.Errorf("links to bob's agent or desktop for the admin:\n%s", page)
	}
}

// A member of the IdP not in the users file sees no VM and is told why.
func TestPanelUnlinkedMember(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("stranger")
	code, page := b.page("/")
	if code != 200 || !strings.Contains(page, "stranger@example.com") || !strings.Contains(page, "admin") {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, vm := range []string{"test", "bobs-vm", "mine"} {
		if strings.Contains(page, ">"+vm+"<") {
			t.Errorf("sees %s", vm)
		}
	}
	if code, _ := b.page("/vm/test"); code != 404 {
		t.Errorf("detail of someone's VM: %d", code)
	}
}

// Who I am for boxe, my keys, how to set up SSH, and logging out.
func TestPanelMe(t *testing.T) {
	pt := newPanelTest(t)
	code, page := pt.browser("member").page("/me")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{"alice@example.com", "alice", "AAAAC3NzaC1lZDI1NTE5AAAAIAlice", "ssh boxe ssh-config",
		"ssh boxe known-hosts", `method="post"`, `action="/logout"`} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
}

// A session on a VM host is no session on the panel: VMs are untrusted.
func TestPanelIgnoresVMSessions(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.web.sessionCookie("alice@example.com", "test.boxe.example", time.Now().UnixMilli())
	b := pt.browser("member")
	res := b.do("https://boxe.example/", http.Header{"Cookie": {c.Name + "=" + c.Value}})
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "/__boxe/login") {
		t.Errorf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// Every page of the panel locks down what the browser may do with it, and
// carries no inline script or style, which the policy forbids.
func TestPanelSecurityHeaders(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	b.page("/")
	for _, path := range []string{"/", "/vm/test", "/me", "/nope", "/static/panel.css"} {
		res := b.do("https://boxe.example"+path, nil)
		h := res.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'self'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: %v", path, h)
		}
		if strings.HasPrefix(path, "/static/") {
			continue
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, h.Get("Cache-Control"))
		}
		page := body(res)
		if strings.Contains(page, "<script>") || strings.Contains(page, "style=") || strings.Contains(page, "<style") || strings.Contains(page, "onclick=") {
			t.Errorf("%s: inline script or style:\n%s", path, page)
		}
	}
	if res := b.do("https://boxe.example/healthz", nil); res.StatusCode != 200 || body(res) != "ok\n" {
		t.Errorf("healthz: %d", res.StatusCode)
	}
}

// The browser asks for /favicon.ico on its own, also during a login: it
// gets the icon, and no new login that would replace the one going on.
func TestPanelFaviconNeedsNoLogin(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	res := b.do("https://boxe.example/favicon.ico", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || len(res.Cookies()) != 0 {
		t.Errorf("%d %q, cookies %v", res.StatusCode, res.Header.Get("Content-Type"), res.Cookies())
	}
}
