package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --agent <prompt>, last on the line, starts a Shelley conversation in the
// new VM with the rest of the line as it was typed, and prints its link.
func TestAgentFlag(t *testing.T) {
	c := newCLI(t, "alice")
	out := c.must("alice", `new test --agent fix the bug, don't break "anything"`)
	if !strings.Contains(out, "https://test--agent.boxe.example.com/c/fix-the-bug") {
		t.Errorf("no link to the conversation:\n%s", out)
	}
	var chat string
	for _, cmd := range c.host.cmds {
		if strings.Contains(cmd, "shelley client chat") {
			chat = cmd
		}
	}
	if !strings.Contains(chat, "exedev@10.42.0.2") || !strings.Contains(chat, `-p 'fix the bug, don'\''t break "anything"'`) {
		t.Errorf("chat started with %q", chat)
	}

	// Copies too, where the member's own keys for the models come along.
	var v vmJSON
	c.json("alice", "cp test copy --json --agent go on", &v)
	if v.Agent != "https://copy--agent.boxe.example.com/c/fix-the-bug" {
		t.Errorf("cp --json: agent %q", v.Agent)
	}
	c.must("alice", "fork test fork1 --agent go on")

	if _, errOut, ok := c.run("alice", "new t2 --agent   "); ok || !strings.Contains(errOut, "prompt") {
		t.Errorf("empty prompt: %q", errOut)
	}
	// Without a model of the member's own, the agent cannot work: they are
	// told where to add their key.
	c.host.noModels = true
	_, errOut, ok := c.run("alice", "new t4 --agent hi")
	if ok || !strings.Contains(errOut, "https://t4--agent.boxe.example.com") || !strings.Contains(errOut, "key") {
		t.Errorf("no models: %q", errOut)
	}
	c.host.noModels = false
	// The VM is there even if the agent does not start.
	c.host.noShelley = true
	if _, errOut, ok := c.run("alice", "new t3 --agent hi"); ok || !strings.Contains(errOut, "t3 is ready") {
		t.Errorf("agent down: %q", errOut)
	}
	if _, err := c.box.store.Get("t3"); err != nil {
		t.Error("the VM was deleted")
	}
}

// The second column of the users file is the member's email at the IdP.
func TestReadUsersWithEmails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users")
	os.WriteFile(p, []byte("# team\nalice alice@example.com\nbob\n\n"), 0o644)
	users, emails, err := readUsers(p)
	if err != nil || strings.Join(users, ",") != "alice,bob" {
		t.Fatalf("users %v, %v", users, err)
	}
	if emails["alice"] != "alice@example.com" || emails["bob"] != "" {
		t.Errorf("emails %v", emails)
	}
}

// https://<vm>--agent.<domain> is Shelley, the VM's agent: a shell in the
// VM, so only its owner gets in, and pages of other VMs (same site) cannot
// make the owner's browser drive it.
func TestWebAgentIsTheOwners(t *testing.T) {
	wt := newWebTest(t)
	owners := map[string]string{"alice": "alice@example.com"}
	wt.web.ownerEmail = func(u string) string { return owners[u] }
	b := wt.browser("member")
	res := b.get("https://test--agent.boxe.example/", nil)
	if res.StatusCode != 200 || wt.port != agentWebPort {
		t.Fatalf("owner: %d, port %d", res.StatusCode, wt.port)
	}
	if got := wt.seen.Header.Get("X-Box-Email"); got != "alice@example.com" {
		t.Errorf("X-Box-Email %q", got)
	}
	if res := b.send("POST", "https://test--agent.boxe.example/api/chat", http.Header{"Sec-Fetch-Site": {"same-origin"}}); res.StatusCode != 200 {
		t.Errorf("owner's POST: %d", res.StatusCode)
	}
	if res := b.send("POST", "https://test--agent.boxe.example/api/chat", http.Header{"Sec-Fetch-Site": {"same-site"}, "Origin": {"https://other.boxe.example"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST from another VM's page: %d, want 403", res.StatusCode)
	}
	// Other ports stay open to the team.
	owners["alice"] = "boss@example.com"
	if res := b.get("https://test--3000.boxe.example/", nil); res.StatusCode != 200 {
		t.Errorf("port 3000 for a member: %d", res.StatusCode)
	}
	for _, email := range []string{"boss@example.com", ""} {
		owners["alice"] = email
		wt.seen = nil
		res := b.get("https://test--agent.boxe.example/", nil)
		if res.StatusCode != http.StatusForbidden || wt.seen != nil {
			t.Errorf("owner %q: %d, reached the VM: %v", email, res.StatusCode, wt.seen != nil)
		}
	}
}

// The prompt goes to the VM it was given for, never to one that took its
// place while it booted: same name and address, but someone else's.
func TestAgentNotSentToAReplacedVM(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.host.onSSH = func() {
		c.must("alice", "rm test")
		c.must("bob", "new test")
	}
	if _, errOut, ok := c.run("alice", "new test --agent read the secrets"); ok || !strings.Contains(errOut, "deleted") {
		t.Errorf("alice: %q", errOut)
	}
	for _, cmd := range c.host.cmds {
		if strings.Contains(cmd, "shelley client chat") {
			t.Errorf("the prompt reached bob's VM: %s", cmd)
		}
	}
}

// The host's own SSH into a VM (agent, sync) checks the VM's certificate
// for its name: an address now used by another VM is refused.
func TestHostSSHChecksTheVM(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test --agent hi")
	var chat string
	for _, cmd := range c.host.cmds {
		if strings.Contains(cmd, "shelley client chat") {
			chat = cmd
		}
	}
	for _, want := range []string{"StrictHostKeyChecking=yes", "HostKeyAlias=test.boxe", "UserKnownHostsFile=/"} {
		if !strings.Contains(chat, want) {
			t.Errorf("ssh lacks %s: %s", want, chat)
		}
	}
}
