package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// cli is boxed as its users see it: `ssh boxe <command>`, over a real SSH
// connection, with the host's commands faked.
type cli struct {
	t    *testing.T
	box  *Box
	host *fakeHost
	conn map[string]*gossh.Client // user -> connection
}

func newCLI(t *testing.T, users ...string) *cli {
	t.Helper()
	h, f := newFakeHypervisor(t)
	h.SSHSuffix, h.WebDomain = "boxe", "boxe.example.com"
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	signers := map[string]gossh.Signer{}
	for _, u := range users {
		_, sk, _ := ed25519.GenerateKey(rand.Reader)
		signers[u], _ = gossh.NewSignerFromKey(sk)
		byKey[string(signers[u].PublicKey().Marshal())] = u
	}
	keys := &Keys{store: s}
	keys.set(byKey, map[string]string{})
	b := &Box{store: s, keys: keys, hv: h, name: "boxe"}
	srv := b.server()
	hostKey, _ := loadHostKey(filepath.Join(t.TempDir(), "host"))
	srv.AddHostKey(hostKey)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c := &cli{t: t, box: b, host: f, conn: map[string]*gossh.Client{}}
	for _, u := range users {
		conn, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{User: u,
			Auth: []gossh.AuthMethod{gossh.PublicKeys(signers[u])}, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		c.conn[u] = conn
	}
	return c
}

// run is `ssh boxe <cmd>` as user: what it prints and whether it succeeded.
func (c *cli) run(user, cmd string) (stdout, stderr string, ok bool) {
	c.t.Helper()
	s, err := c.conn[user].NewSession()
	if err != nil {
		c.t.Fatal(err)
	}
	defer s.Close()
	var o, e bytes.Buffer
	s.Stdout, s.Stderr = &o, &e
	err = s.Run(cmd)
	var exit *gossh.ExitError
	if err != nil && !errors.As(err, &exit) {
		c.t.Fatalf("%s: %v", cmd, err)
	}
	return o.String(), e.String(), err == nil
}

// must runs cmd and fails the test if it does not succeed.
func (c *cli) must(user, cmd string) string {
	c.t.Helper()
	out, errOut, ok := c.run(user, cmd)
	if !ok {
		c.t.Fatalf("%s: %s%s", cmd, out, errOut)
	}
	return out
}

func (c *cli) json(user, cmd string, v any) {
	c.t.Helper()
	out := c.must(user, cmd)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		c.t.Fatalf("%s: not JSON: %v\n%s", cmd, err, out)
	}
}

func TestJSONOutput(t *testing.T) {
	c := newCLI(t, "alice")
	var created vmJSON
	c.json("alice", "new --json test --mem 1G", &created)
	if created.Name != "test" || created.SSH != "test.boxe" || created.URL != "https://test.boxe.example.com" ||
		created.IP != "10.42.0.2" || created.CPUs != 2 || created.Mem != "1G" || created.State != "running" || created.Owner != "alice" {
		t.Errorf("new --json: %+v", created)
	}
	var copied vmJSON
	c.json("alice", "cp test copy --json", &copied)
	if copied.Name != "copy" || copied.Mem != "1G" {
		t.Errorf("cp --json: %+v", copied)
	}
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	if len(list) != 2 || list[0].Name != "copy" || list[1].Name != "test" {
		t.Errorf("ls --json: %+v", list)
	}
	var who struct{ User string }
	c.json("alice", "whoami --json", &who)
	if who.User != "alice" {
		t.Errorf("whoami --json: %+v", who)
	}
	var done struct{ Name, Status string }
	c.json("alice", "restart --json test", &done)
	if done.Name != "test" || done.Status != "restarted" {
		t.Errorf("restart --json: %+v", done)
	}
	c.json("alice", "rm --json copy", &done)
	if done.Name != "copy" || done.Status != "deleted" {
		t.Errorf("rm --json: %+v", done)
	}
	// An empty list is [], not null.
	c.must("alice", "rm test")
	if out := c.must("alice", "ls --json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("empty ls --json: %q", out)
	}
}

// Errors are JSON too, on stdout, and the command still fails.
func TestJSONErrors(t *testing.T) {
	c := newCLI(t, "alice")
	out, _, ok := c.run("alice", "rm --json nope")
	if ok {
		t.Fatal("rm of a missing VM succeeded")
	}
	var e struct{ Error string }
	if err := json.Unmarshal([]byte(out), &e); err != nil || !strings.Contains(e.Error, "no VM named nope") {
		t.Errorf("error as JSON: %q (%v)", out, err)
	}
}

// Without --json, the output stays for people.
func TestTextOutput(t *testing.T) {
	c := newCLI(t, "alice")
	out := c.must("alice", "new test")
	if !strings.Contains(out, "test ready in") || !strings.Contains(out, "ssh test.boxe") {
		t.Errorf("new: %q", out)
	}
	if out := c.must("alice", "ls"); !strings.HasPrefix(out, "NAME") {
		t.Errorf("ls: %q", out)
	}
}

func TestTTL(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new keep")
	var v vmJSON
	c.json("alice", "new temp --ttl 2h --json", &v)
	exp, err := time.Parse(time.RFC3339, v.Expires)
	if err != nil || exp.Sub(time.Now()) < 119*time.Minute || exp.Sub(time.Now()) > 121*time.Minute {
		t.Fatalf("new --ttl 2h: expires %q", v.Expires)
	}
	c.json("alice", "cp keep temp2 --ttl 1d --json", &v)
	if exp, _ := time.Parse(time.RFC3339, v.Expires); exp.Sub(time.Now()) < 23*time.Hour {
		t.Errorf("cp --ttl 1d: expires %q", v.Expires)
	}
	if out := c.must("alice", "ls"); !strings.Contains(out, "EXPIRES") || !strings.Contains(out, "in 1h59m") && !strings.Contains(out, "in 2h0m") {
		t.Errorf("ls does not show when temp expires:\n%s", out)
	}

	// A member can change it or remove it.
	c.must("alice", "ttl temp 30m")
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	byName := map[string]vmJSON{}
	for _, v := range list {
		byName[v.Name] = v
	}
	if exp, _ := time.Parse(time.RFC3339, byName["temp"].Expires); exp.Sub(time.Now()) > 31*time.Minute {
		t.Errorf("ttl 30m: expires %q", byName["temp"].Expires)
	}
	if byName["keep"].Expires != "" {
		t.Errorf("a VM without --ttl expires: %q", byName["keep"].Expires)
	}
	c.must("alice", "ttl temp2 off")

	// Once expired, the VM is deleted like with rm.
	c.box.reap(time.Now().Add(time.Hour))
	c.json("alice", "ls --json", &list)
	if len(list) != 2 || list[0].Name != "keep" || list[1].Name != "temp2" {
		t.Errorf("after expiry: %+v", list)
	}
	if !c.host.ran("zfs destroy -r boxe/vms/temp") {
		t.Error("the expired VM's disk is still there")
	}
}

func TestTTLErrors(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	for _, cmd := range []string{"new --ttl soon", "new --ttl 0m", "new --ttl 1s", "new --ttl 400d"} {
		if _, _, ok := c.run("alice", cmd); ok {
			t.Errorf("%s: accepted", cmd)
		}
	}
	c.must("alice", "new mine")
	if _, _, ok := c.run("bob", "ttl mine 1h"); ok {
		t.Error("bob changed the ttl of alice's VM")
	}
}
