package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// prompt is `ssh boxe` from a terminal: a session with a pty and a shell.
type prompt struct {
	t   *testing.T
	s   *gossh.Session
	in  io.WriteCloser
	mu  sync.Mutex
	out bytes.Buffer
}

func (c *cli) prompt(user string) *prompt {
	c.t.Helper()
	s, err := c.conn[user].NewSession()
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { s.Close() })
	if err := s.RequestPty("xterm", 40, 200, gossh.TerminalModes{}); err != nil {
		c.t.Fatal(err)
	}
	p := &prompt{t: c.t, s: s}
	p.in, _ = s.StdinPipe()
	stdout, _ := s.StdoutPipe()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if err := s.Shell(); err != nil {
		c.t.Fatal(err)
	}
	p.wait("boxe ▶ ")
	return p
}

// wait returns what the terminal shows once it shows want, and clears it.
func (p *prompt) wait(want string) string {
	p.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		got := p.out.String()
		if strings.Contains(got, want) {
			p.out.Reset()
			p.mu.Unlock()
			return got
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.t.Fatalf("waiting for %q, the terminal shows:\n%s", want, p.out.String())
	return ""
}

func (p *prompt) typ(keys string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, keys); err != nil {
		p.t.Fatal(err)
	}
}

// do types a command and returns what it prints before the next prompt.
func (p *prompt) do(line string) string {
	p.t.Helper()
	p.typ(line + "\r")
	return p.wait("boxe ▶ ")
}

func TestPrompt(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	p := c.prompt("alice")

	if out := p.do("help"); !strings.Contains(out, "  new [name]") || strings.Contains(out, "ssh boxe new") {
		t.Errorf("help in the prompt should list bare commands:\n%s", out)
	}
	if out := p.do("new web"); !strings.Contains(out, "web") {
		t.Errorf("new web:\n%s", out)
	}
	if out := p.do("ls"); !strings.Contains(out, "web") {
		t.Errorf("ls after new:\n%s", out)
	}
	if out := p.do("ls --json"); !strings.Contains(out, `"name":"web"`) {
		t.Errorf("ls --json:\n%s", out)
	}
	if out := p.do("frobnicate"); !strings.Contains(out, "error: unknown command: frobnicate") {
		t.Errorf("an unknown command should say so and keep the prompt:\n%s", out)
	}
	if out := p.do(""); strings.Contains(out, "error") {
		t.Errorf("an empty line should do nothing:\n%s", out)
	}
	if out := p.do("whoami"); !strings.Contains(out, "alice") {
		t.Errorf("whoami:\n%s", out)
	}

	p.typ("exit\r")
	if err := p.s.Wait(); err != nil {
		t.Errorf("exit should end the session cleanly: %v", err)
	}
}

func TestPromptCompletes(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new web")
	c.must("bob", "new website")
	p := c.prompt("alice")

	// Commands, then the user's own VMs.
	p.typ("unsh\tweb\r")
	if out := p.wait("boxe ▶ "); !strings.Contains(out, "unshare web") {
		t.Errorf("tab should complete unsh to unshare:\n%s", out)
	}
	p.typ("rm w\t\r")
	p.wait("boxe ▶ ")
	if out := c.must("alice", "ls"); strings.Contains(out, "web") {
		t.Errorf("rm w<tab> should have completed to web and deleted it:\n%s", out)
	}
	if out := c.must("bob", "ls"); !strings.Contains(out, "website") {
		t.Errorf("bob's VM should be untouched:\n%s", out)
	}
}

func TestPromptEndsOnCtrlD(t *testing.T) {
	c := newCLI(t, "alice")
	p := c.prompt("alice")
	p.typ("\x04")
	if err := p.s.Wait(); err != nil {
		t.Errorf("ctrl-d should end the session cleanly: %v", err)
	}
}

func TestNoPromptWithoutTerminal(t *testing.T) {
	c := newCLI(t, "alice")
	out, _, ok := c.run("alice", "")
	if !ok || !strings.Contains(out, "ssh boxe new") {
		t.Errorf("ssh boxe from a script should print help and exit, got ok=%v:\n%s", ok, out)
	}
}
