package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// exec is POST /exec with token (none if empty) and the command line, as a
// program sends it: no cookies.
func (pt *panelTest) exec(token, line string, header http.Header) (int, string, http.Header) {
	pt.t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		h[k] = v
	}
	res := pt.browser("nobody").sendBody("POST", "https://boxe.example/exec", h, line)
	return res.StatusCode, body(res), res.Header
}

// A member makes a token over SSH, sees it once, lists it without it and
// deletes it.
func TestTokenCommands(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.c
	var tok tokenJSON
	c.json("alice", "token new --label ci --cmds ls,new --json", &tok)
	if !strings.HasPrefix(tok.Token, "boxe_") || len(tok.Token) < 40 || tok.ID == "" || tok.Label != "ci" ||
		strings.Join(tok.Commands, ",") != "ls,new" {
		t.Fatalf("token new: %+v", tok)
	}
	// 30 days unless told otherwise.
	if exp, err := time.Parse(time.RFC3339, tok.Expires); err != nil || exp.Sub(time.Now()) < 29*24*time.Hour || exp.Sub(time.Now()) > 31*24*time.Hour {
		t.Errorf("default expiry: %q", tok.Expires)
	}
	out := c.must("alice", "token new --exp 2h")
	if !strings.Contains(out, "boxe_") || !strings.Contains(out, "Authorization: Bearer boxe_") || !strings.Contains(out, "https://boxe.example/exec") {
		t.Errorf("token new:\n%s", out)
	}
	list := c.must("alice", "token ls")
	if strings.Contains(list, tok.Token) || strings.Contains(list, "boxe_") {
		t.Errorf("token ls shows a token:\n%s", list)
	}
	for _, want := range []string{"ID", "LABEL", "COMMANDS", "LAST USED", tok.ID, "ci", "ls,new", "all", "never"} {
		if !strings.Contains(list, want) {
			t.Errorf("token ls: no %q in:\n%s", want, list)
		}
	}
	var tokens []tokenJSON
	c.json("alice", "token ls --json", &tokens)
	if len(tokens) != 2 || tokens[0].Token != "" || tokens[1].Token != "" {
		t.Errorf("token ls --json: %+v", tokens)
	}
	// Someone else's tokens are not theirs to see or delete.
	if out := c.must("bob", "token ls"); strings.Contains(out, tok.ID) {
		t.Errorf("bob sees alice's token:\n%s", out)
	}
	if _, _, ok := c.run("bob", "token rm ci"); ok {
		t.Error("bob deleted alice's token")
	}
	for _, bad := range []string{"token new --exp 0m", "token new --exp never", "token new --exp 400d", "token new --cmds ls,token",
		"token new --cmds nope", "token new --label ci", "token new --label 'a b'", "token new extra", "token", "token rm"} {
		if _, _, ok := c.run("alice", bad); ok {
			t.Errorf("%s: accepted", bad)
		}
	}

	if code, _, _ := pt.exec(tok.Token, "ls", nil); code != 200 {
		t.Fatalf("the token does not work: %d", code)
	}
	c.json("alice", "token ls --json", &tokens)
	if tokens[0].LastUsed == "" && tokens[1].LastUsed == "" {
		t.Errorf("last use not recorded: %+v", tokens)
	}
	c.must("alice", "token rm ci")
	if code, _, _ := pt.exec(tok.Token, "ls", nil); code != http.StatusUnauthorized {
		t.Errorf("a deleted token works: %d", code)
	}
	c.json("alice", "token ls --json", &tokens)
	if len(tokens) != 1 {
		t.Errorf("after rm: %+v", tokens)
	}
	c.must("alice", "token rm "+tokens[0].ID)
}

// POST /exec runs a command as over SSH, as the token's member, and answers
// what it printed.
func TestAPIExec(t *testing.T) {
	pt := newPanelTest(t)
	var tok tokenJSON
	pt.c.json("alice", "token new --json", &tok)

	code, out, h := pt.exec(tok.Token, "ls --json", nil)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || out != pt.c.must("alice", "ls --json") {
		t.Fatalf("ls --json: %d %q\n%s", code, h.Get("Content-Type"), out)
	}
	code, out, _ = pt.exec(tok.Token, "new web --mem 1G --json\n", nil)
	var v vmJSON
	if code != 200 || json.Unmarshal([]byte(out), &v) != nil || v.Name != "web" || v.Owner != "alice" || v.Mem != "1G" {
		t.Fatalf("new: %d\n%s", code, out)
	}
	if code, out, _ := pt.exec(tok.Token, "whoami", nil); code != 200 || out != "alice\n" {
		t.Errorf("whoami: %d %q", code, out)
	}

	// Mistakes are the member's: 400 with what ssh boxe says.
	if code, out, _ := pt.exec(tok.Token, "rm nope", nil); code != http.StatusBadRequest || !strings.Contains(out, "no VM named nope") {
		t.Errorf("rm nope: %d %q", code, out)
	}
	if code, out, _ := pt.exec(tok.Token, "rm bobs-vm --json", nil); code != http.StatusBadRequest || !strings.Contains(out, `"error"`) || !pt.exists("bobs-vm") {
		t.Errorf("rm bobs-vm: %d %q", code, out)
	}
	// Failures on the host are not: 500.
	pt.c.host.fail["zfs clone"] = true
	if code, out, _ := pt.exec(tok.Token, "new broken", nil); code != http.StatusInternalServerError || !strings.Contains(out, "could not create the VM") {
		t.Errorf("failed new: %d %q", code, out)
	}
	delete(pt.c.host.fail, "zfs clone")

	// Tokens never make tokens.
	if code, _, _ := pt.exec(tok.Token, "token new", nil); code != http.StatusForbidden {
		t.Errorf("token new over the API: %d", code)
	}
	if code, _, _ := pt.exec(tok.Token, "token ls --json", nil); code != http.StatusForbidden {
		t.Errorf("token ls over the API: %d", code)
	}
}

// Only a valid token of a member of the team gets in; a session cookie is
// no way in, so pages of VMs cannot use the API with a member's browser.
func TestAPIAuth(t *testing.T) {
	pt := newPanelTest(t)
	var tok, limited tokenJSON
	pt.c.json("alice", "token new --json", &tok)
	pt.c.json("alice", "token new --cmds ls,whoami --json", &limited)

	if code, _, h := pt.exec("", "ls", nil); code != http.StatusUnauthorized || !strings.HasPrefix(h.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no token: %d %v", code, h)
	}
	if code, _, _ := pt.exec("boxe_nope", "ls", nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code, _, _ := pt.exec("", "ls", http.Header{"Authorization": {"Basic " + tok.Token}}); code != http.StatusUnauthorized {
		t.Errorf("not a bearer token: %d", code)
	}
	// The member's session does nothing here.
	b := pt.browser("member")
	b.page("/")
	res := b.sendBody("POST", "https://boxe.example/exec", fromPanel, "ls")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("with the panel's cookie: %d", res.StatusCode)
	}
	if res := pt.browser("nobody").send("GET", "https://boxe.example/exec", http.Header{"Authorization": {"Bearer " + tok.Token}}); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", res.StatusCode)
	}

	// --cmds limits the commands.
	if code, _, _ := pt.exec(limited.Token, "ls --json", nil); code != 200 {
		t.Errorf("limited ls: %d", code)
	}
	for _, line := range []string{"new x", "rm test", "exec test id", "", "help", "cp test x --agent ls"} {
		if code, _, _ := pt.exec(limited.Token, line, nil); code != http.StatusForbidden {
			t.Errorf("limited %q: %d", line, code)
		}
	}
	if !pt.exists("test") || pt.exists("x") {
		t.Error("a limited token changed VMs")
	}

	// Too big a command line.
	if code, _, _ := pt.exec(tok.Token, "ls "+strings.Repeat("x", 100<<10), nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: %d", code)
	}

	// Expired.
	pt.c.box.store.db.Exec("UPDATE tokens SET expires_at = ? WHERE id = ?", timeCol(time.Now().Add(-time.Minute)), tok.ID)
	if code, out, _ := pt.exec(tok.Token, "ls", nil); code != http.StatusUnauthorized || !strings.Contains(out, "expired") {
		t.Errorf("expired: %d %q", code, out)
	}

	// Who leaves the team loses their tokens with it.
	pt.c.box.keys.mu.Lock()
	pt.c.box.keys.users = []string{"bob", "dave"}
	pt.c.box.keys.mu.Unlock()
	if code, _, _ := pt.exec(limited.Token, "ls", nil); code != http.StatusForbidden {
		t.Errorf("after leaving: %d", code)
	}
}

// exitCode is how the fake host fails a command with an exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

// exec runs a command in the member's own VM, over SSH and over the API,
// waking it if it sleeps, and fails as the command did.
func TestExecInVM(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.c
	var ran []string
	c.host.inVM = func(cmd string) ([]byte, error) {
		ran = append(ran, cmd)
		switch cmd {
		case "false":
			return []byte("nope\n"), exitCode(3)
		case "unreachable":
			return []byte("ssh: connect to host: Connection refused\n"), exitCode(255)
		}
		return []byte("out of " + cmd + "\n"), nil
	}

	if out := c.must("alice", `exec test echo "a  b" | wc -c`); out != "out of echo \"a  b\" | wc -c\n" {
		t.Errorf("exec over SSH: %q", out)
	}
	if !c.host.ran("ssh -i") || !c.host.ran("ssh -i "+c.box.hv.HostKeyPath) {
		t.Errorf("not run with the host's key: %v", c.host.cmds)
	}
	// The exit status is the command's, with nothing added.
	s, _ := c.conn["alice"].NewSession()
	var o, e strings.Builder
	s.Stdout, s.Stderr = &o, &e
	err := s.Run("exec test false")
	var exit interface{ ExitStatus() int }
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || o.String() != "nope\n" || e.String() != "" {
		t.Errorf("exec false: %v %q %q", err, o.String(), e.String())
	}
	s.Close()

	// --json, --agent and lone quotes belong to the command.
	for _, cmd := range []string{"curl --json x --agent y", "echo it's here"} {
		c.must("alice", "exec test "+cmd)
		if ran[len(ran)-1] != cmd {
			t.Errorf("command changed on its way: %q, not %q", ran[len(ran)-1], cmd)
		}
	}

	// Asleep, the VM wakes up first.
	c.must("alice", "sleep test")
	if out := c.must("alice", "exec test uptime"); out != "out of uptime\n" || c.box.hv.Sleeping("test") {
		t.Errorf("exec on a sleeping VM: %q, sleeping %v", out, c.box.hv.Sleeping("test"))
	}

	// Only in one's own VMs, an admin's included.
	n := len(ran)
	for _, cmd := range []struct{ user, line string }{{"bob", "exec test id"}, {"dave", "exec test id"}, {"alice", "exec nope id"},
		{"alice", "exec test"}, {"alice", "exec"}} {
		if _, _, ok := c.run(cmd.user, cmd.line); ok {
			t.Errorf("%s: %s succeeded", cmd.user, cmd.line)
		}
	}
	if len(ran) != n {
		t.Errorf("ran in a VM: %v", ran[n:])
	}
	if _, errOut, ok := c.run("alice", "exec test unreachable"); ok || !strings.Contains(errOut, "could not reach test") {
		t.Errorf("unreachable VM: %v %q", ok, errOut)
	}

	var tok tokenJSON
	c.json("alice", "token new --json", &tok)
	if code, out, h := pt.exec(tok.Token, "exec test cat /etc/hostname\nhostname", nil); code != 200 || out != "out of cat /etc/hostname\nhostname\n" || h.Get("X-Exit-Status") != "0" {
		t.Errorf("exec over the API: %d %q %v", code, out, h)
	}
	if code, out, h := pt.exec(tok.Token, "exec test false", nil); code != http.StatusUnprocessableEntity || out != "nope\n" || h.Get("X-Exit-Status") != "3" {
		t.Errorf("failing exec over the API: %d %q %v", code, out, h)
	}
	if code, _, _ := pt.exec(tok.Token, "exec bobs-vm id", nil); code != http.StatusBadRequest {
		t.Errorf("exec in bob's VM over the API: %d", code)
	}
}

// An admin can restart and delete anyone's VM from the CLI, as from the
// panel, and is told whose it was; members only their own.
func TestAdminManagesOthersVMs(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.c
	for _, cmd := range []string{"restart bobs-vm", "rm bobs-vm"} {
		if _, _, ok := c.run("alice", cmd); ok {
			t.Errorf("alice: %s succeeded", cmd)
		}
	}
	if out := c.must("dave", "restart bobs-vm"); !strings.Contains(out, "bobs-vm restarted") || !strings.Contains(out, "bob") {
		t.Errorf("admin restart: %q", out)
	}
	var d struct{ Name, Status, Owner string }
	c.json("dave", "rm bobs-vm --json", &d)
	if d.Name != "bobs-vm" || d.Status != "deleted" || d.Owner != "bob" || pt.exists("bobs-vm") {
		t.Errorf("admin rm: %+v", d)
	}
	// Still owner-only: everything else.
	for _, cmd := range []string{"cp test x", "ttl test 1h", "sleep test", "share test", "exec test id"} {
		if _, _, ok := c.run("dave", cmd); ok {
			t.Errorf("dave: %s succeeded", cmd)
		}
	}
}

// The prompt completes the new commands too.
func TestPromptKnowsNewCommands(t *testing.T) {
	c := newCLI(t, "alice")
	for _, cmd := range []string{"exec", "token"} {
		if line, _, ok := c.box.complete("alice", cmd[:3], 3); !ok || line != cmd+" " {
			t.Errorf("%s: %q %v", cmd[:3], line, ok)
		}
	}
	if h := c.must("alice", "help"); !strings.Contains(h, "exec <name> <command>") || !strings.Contains(h, "token new") {
		t.Errorf("help:\n%s", h)
	}
}
