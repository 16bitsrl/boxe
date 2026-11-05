package main

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// team makes alice, bob and carol the members of c's team, each with a key.
func team(c *cli) {
	c.box.keys.mu.Lock()
	defer c.box.keys.mu.Unlock()
	c.box.keys.users = []string{"alice", "bob", "carol"}
	c.box.keys.byUser = map[string]string{
		"alice": "ssh-ed25519 AAAAalice alice@laptop\n",
		"bob":   "ssh-ed25519 AAAAbob bob@laptop\n",
		"carol": "ssh-ed25519 AAAAcarol carol@laptop\n",
	}
}

// `ssh boxe access <vm> add|rm <member|team>`: the owner lets other members
// into a VM. Only the owner gives or takes access; the VM's own users see
// who has it.
func TestAccessCommand(t *testing.T) {
	c := newCLI(t, "alice", "bob", "carol")
	team(c)
	c.must("alice", "new test")

	if out := c.must("alice", "access test"); !strings.Contains(out, "alice") || strings.Contains(out, "bob") {
		t.Errorf("access before sharing: %q", out)
	}
	if out := c.must("bob", "ls"); strings.Contains(out, "test") {
		t.Errorf("bob sees alice's VM: %q", out)
	}
	c.must("alice", "access test add bob")
	if out := c.must("bob", "ls"); !strings.Contains(out, "test") || !strings.Contains(out, "alice") {
		t.Errorf("bob's ls after access add: %q", out)
	}
	var j struct {
		Name  string   `json:"name"`
		Owner string   `json:"owner"`
		Users []string `json:"users"`
	}
	c.json("bob", "access test --json", &j)
	if j.Name != "test" || j.Owner != "alice" || !slices.Equal(j.Users, []string{"bob"}) {
		t.Errorf("access --json: %+v", j)
	}
	if out := c.must("carol", "ls"); strings.Contains(out, "test") {
		t.Errorf("carol sees the VM shared with bob: %q", out)
	}
	for user, bad := range map[string][]string{
		"alice": {"access test add nobody", "access test add alice", "access test give bob", "access", "access nope add bob"},
		"bob":   {"access test add carol", "access test rm bob"},
		"carol": {"access test"},
	} {
		for _, cmd := range bad {
			if _, _, ok := c.run(user, cmd); ok {
				t.Errorf("%s: %s succeeded", user, cmd)
			}
		}
	}

	c.must("alice", "access test add team")
	if out := c.must("carol", "ls"); !strings.Contains(out, "test") {
		t.Errorf("carol's ls with access for the team: %q", out)
	}
	c.must("alice", "access test rm team")
	c.must("alice", "access test rm bob")
	if out := c.must("bob", "ls"); strings.Contains(out, "test") {
		t.Errorf("bob's ls after access rm: %q", out)
	}

	// A new VM with the name of a deleted one starts without access.
	c.must("alice", "access test add bob")
	c.must("alice", "rm test")
	c.must("alice", "new test")
	if out := c.must("bob", "ls"); strings.Contains(out, "test") {
		t.Errorf("bob kept access to a new VM with the same name: %q", out)
	}
}

// A member with access uses the VM: SSH, exec, wake. Managing it stays the
// owner's.
func TestAccessUse(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	team(c)
	c.must("alice", "new test")
	if conn, err := c.conn["bob"].Dial("tcp", "test.boxe:22"); err == nil {
		conn.Close()
		t.Fatal("bob reached alice's VM without access")
	}
	c.must("alice", "access test add bob")
	c.must("alice", "sleep test")
	conn, err := c.conn["bob"].Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatalf("bob with access: %v", err)
	}
	conn.Close()
	if s := c.box.hv.State("test"); s != "running" {
		t.Errorf("bob's jump left the VM %s", s)
	}
	c.must("bob", "wake test")
	c.must("bob", "exec test true")
	for _, cmd := range []string{"rm test", "restart test", "ttl test 2h", "share test", "unshare test", "keep-awake test on",
		"egress test all", "sleep test", "cp test copy", "fork test copy"} {
		if _, _, ok := c.run("bob", cmd); ok {
			t.Errorf("bob ran %s on alice's VM", cmd)
		}
	}
	c.must("alice", "access test rm bob")
	if conn, err := c.conn["bob"].Dial("tcp", "test.boxe:22"); err == nil {
		conn.Close()
		t.Error("bob reached the VM after access rm")
	}
}

// The VM's sshd asks boxe for the keys that may log in as boxedev: the
// owner's and those of the members with access, as they are now.
func TestAccessSSHKeys(t *testing.T) {
	c := newCLI(t, "alice", "bob", "carol")
	team(c)
	c.must("alice", "new test") // 10.42.0.2
	_, client := serveProxy(t, testProxy(c))
	keys := func(user string) string {
		t.Helper()
		res, err := client.Get("http://10.42.0.1/ssh-keys?user=" + user)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("ssh-keys: %d %s", res.StatusCode, b)
		}
		return string(b)
	}
	if k := keys("boxedev"); k != "ssh-ed25519 AAAAalice alice@laptop\n" {
		t.Errorf("owner only: %q", k)
	}
	c.must("alice", "access test add bob")
	if k := keys("boxedev"); !strings.Contains(k, "AAAAalice") || !strings.Contains(k, "AAAAbob") || strings.Contains(k, "AAAAcarol") {
		t.Errorf("with bob: %q", k)
	}
	if k := keys("root"); k != "" {
		t.Errorf("keys for root: %q", k)
	}
	c.must("alice", "access test add team")
	if k := keys("boxedev"); strings.Count(k, "AAAAbob") != 1 || !strings.Contains(k, "AAAAcarol") {
		t.Errorf("with the team: %q", k)
	}
	// Who leaves the team loses access with their keys.
	c.box.keys.mu.Lock()
	c.box.keys.users = []string{"alice", "bob"}
	delete(c.box.keys.byUser, "carol")
	c.box.keys.mu.Unlock()
	if k := keys("boxedev"); strings.Contains(k, "AAAAcarol") {
		t.Errorf("carol left the team: %q", k)
	}
}

// New VMs ask boxe for those keys: sshd runs the command in the
// configuration boxe writes.
func TestAccessSSHDConfig(t *testing.T) {
	c := newCLI(t, "alice")
	conf := archiveFiles(t, c.box.hv, VM{Name: "test", Index: 2})["etc/ssh/sshd_config.d/boxe-access.conf"]
	for _, want := range []string{"AuthorizedKeysCommand /usr/bin/curl ", "http://10.42.0.1/ssh-keys?user=%u", "AuthorizedKeysCommandUser nobody"} {
		if !strings.Contains(conf, want) {
			t.Errorf("sshd configuration lacks %q: %q", want, conf)
		}
	}
}

// The agent and the desktop are a shell in the VM: the members with access
// use them as the owner does.
func TestWebAccessAgent(t *testing.T) {
	wt := newWebTest(t)
	wt.web.ownerEmail = func(u string) string { return map[string]string{"alice": "alice@example.com"}[u] }
	wt.web.memberOf = func(email string) (string, bool) {
		return map[string]string{"alice@example.com": "alice", "bob@example.com": "bob"}[email], false
	}
	b := wt.browser("bob")
	if res := b.get("https://test--agent.boxe.example/", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bob without access: %d", res.StatusCode)
	}
	v, _ := wt.web.store.Get("test")
	if err := wt.web.store.Grant(v, "bob"); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"test--agent", "test--desktop"} {
		res := b.get("https://"+host+".boxe.example/", nil)
		if res.StatusCode != 200 {
			t.Errorf("%s for bob with access: %d", host, res.StatusCode)
		}
		if got := wt.seen.Header.Get("X-Box-Email"); got != "bob@example.com" {
			t.Errorf("%s: X-Box-Email %q", host, got)
		}
	}
	if res := b.get("https://other--agent.boxe.example/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("another VM of alice's: %d", res.StatusCode)
	}
}

// In the panel a member with access finds the VM among theirs, with its
// owner, the agent and the desktop; not what manages it.
func TestPanelAccess(t *testing.T) {
	pt := newPanelTest(t)
	pt.c.must("alice", "access test add bob")
	b := pt.browser("bob")
	if code, page := b.page("/"); code != 200 || !strings.Contains(page, `href="/vm/test"`) || !strings.Contains(page, "alice's") {
		t.Errorf("bob's list: %d\n%s", code, page)
	}
	code, page := b.page("/vm/test")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{"https://test--agent.boxe.example", "https://test--desktop.boxe.example"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	if strings.Contains(page, "/restart") || strings.Contains(page, "/console") {
		t.Errorf("bob may manage alice's VM:\n%s", page)
	}
	if code, _ := b.page("/vm/test/console"); code != 404 {
		t.Errorf("console for bob: %d", code)
	}
	_, newPage := b.page("/new")
	token := csrf(t, newPage)
	for _, action := range []string{"restart", "delete", "copy"} {
		if res := b.post("/vm/test/"+action, url.Values{"csrf": {token}, "confirm": {"test"}}, fromPanel); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s by bob: %d", action, res.StatusCode)
		}
	}
	if !pt.exists("test") || pt.c.host.ran("systemctl stop") {
		t.Error("bob touched alice's VM")
	}
	if code, page := pt.browser("member").page("/"); code != 200 || strings.Contains(page, "alice's") {
		t.Errorf("alice's own list: %d\n%s", code, page)
	}
}
