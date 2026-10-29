package main

import (
	"net/http"
	"strings"
	"testing"
)

// `ssh boxe share <vm> [port]` prints a link that shows that port of the
// VM to anyone who has it; `unshare` makes the VM's links stop working.
func TestShareCommand(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test")
	out := c.must("alice", "share test")
	link := strings.TrimSpace(out[strings.Index(out, "https://"):])
	if !strings.HasPrefix(link, "https://test.boxe.example.com/__boxe/share/") || len(link) < len("https://test.boxe.example.com/__boxe/share/")+20 {
		t.Fatalf("share: %q", out)
	}
	if again := c.must("alice", "share test"); !strings.Contains(again, link) {
		t.Errorf("a second share gave another link: %q", again)
	}
	var j struct {
		Name string `json:"name"`
		Port int    `json:"port"`
		URL  string `json:"url"`
	}
	c.json("alice", "share test 3000 --json", &j)
	if j.Name != "test" || j.Port != 3000 || !strings.HasPrefix(j.URL, "https://test--3000.boxe.example.com/__boxe/share/") {
		t.Errorf("share --json: %+v", j)
	}
	for _, bad := range []string{"share test 80", "share test agent", "share test 2999", "share"} {
		if _, _, ok := c.run("alice", bad); ok {
			t.Errorf("%s succeeded", bad)
		}
	}
	if _, errOut, ok := c.run("bob", "share test"); ok || !strings.Contains(errOut, "no VM named test") {
		t.Errorf("bob shared alice's VM: %q", errOut)
	}

	token := link[strings.LastIndex(link, "/")+1:]
	if _, _, _, ok := c.box.store.Shared(token); !ok {
		t.Fatal("the link is not recorded")
	}
	if out := c.must("alice", "unshare test"); !strings.Contains(out, "2 links") {
		t.Errorf("unshare: %q", out)
	}
	if _, _, _, ok := c.box.store.Shared(token); ok {
		t.Error("the link still works after unshare")
	}

	// Deleting the VM ends its links: a new VM with the same name does not
	// inherit them.
	out = c.must("alice", "share test")
	token = strings.TrimSpace(out[strings.LastIndex(out, "/")+1:])
	c.must("alice", "rm test")
	if _, _, _, ok := c.box.store.Shared(token); ok {
		t.Error("the link outlived its VM")
	}
}

// Who opens a share link sees that port of the VM without logging in, as
// nobody: no X-Box-Email. Nothing else of the VM, nor of other VMs.
func TestWebShareLink(t *testing.T) {
	wt := newWebTest(t)
	v, _ := wt.web.store.Get("test")
	token, err := wt.web.store.Share(v, vmWebPort)
	if err != nil {
		t.Fatal(err)
	}
	b := wt.browser("outsider")
	res := b.get("https://test.boxe.example/__boxe/share/"+token, http.Header{"X-Box-Email": {"boss@example.com"}})
	if res.StatusCode != 200 || body(res) != "hello from test.boxe.example" {
		t.Fatalf("share link: %d", res.StatusCode)
	}
	if wt.idp.authorizes.Load() != 0 {
		t.Error("the visitor was sent to log in")
	}
	if wt.seen.URL.Path != "/" {
		t.Errorf("the VM got %s: the token must not reach it", wt.seen.URL.Path)
	}
	if got, ok := wt.seen.Header["X-Box-Email"]; ok {
		t.Errorf("X-Box-Email %q for a visitor", got)
	}
	// Back later, without the link.
	if res := b.get("https://test.boxe.example/page", nil); res.StatusCode != 200 || wt.seen.URL.Path != "/page" {
		t.Errorf("visitor's second page: %d", res.StatusCode)
	}
	for _, other := range []string{"https://other.boxe.example/", "https://test--3000.boxe.example/", "https://test--agent.boxe.example/", "https://test--desktop.boxe.example/"} {
		if res := b.do(other, nil); res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "/__boxe/login") {
			t.Errorf("%s with a link to test: %d, want the login", other, res.StatusCode)
		}
	}
	// A token works only on its own host.
	for _, u := range []string{"https://other.boxe.example/__boxe/share/" + token, "https://test--3000.boxe.example/__boxe/share/" + token, "https://test.boxe.example/__boxe/share/nope"} {
		if res := wt.browser("outsider").do(u, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", u, res.StatusCode)
		}
	}
	// Members keep their identity on a shared VM.
	m := wt.browser("member")
	m.get("https://test.boxe.example/", nil)
	if res := m.get("https://test.boxe.example/__boxe/share/"+token, nil); res.StatusCode != 200 || wt.seen.Header.Get("X-Box-Email") != "alice@example.com" {
		t.Errorf("member with a link: %d, X-Box-Email %q", res.StatusCode, wt.seen.Header.Get("X-Box-Email"))
	}
	if _, err := wt.web.store.Unshare("test"); err != nil {
		t.Fatal(err)
	}
	if res := b.do("https://test.boxe.example/", nil); res.StatusCode != http.StatusFound {
		t.Errorf("after unshare: %d, want the login", res.StatusCode)
	}
}
