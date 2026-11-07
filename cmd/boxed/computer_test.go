package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// fakePNG is a screenshot as boxe-computer encodes it.
func fakePNG(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeComputer answers in the VM as boxe-computer --json does, and records
// what ran there.
func fakeComputer(t *testing.T, c *cli, shot []byte) *[]string {
	ran := &[]string{}
	c.host.inVM = func(cmd string) ([]byte, error) {
		*ran = append(*ran, cmd)
		_, call, _ := strings.Cut(cmd, "boxe-computer --json ")
		switch {
		case strings.HasPrefix(call, "screenshot"), strings.HasPrefix(call, "zoom"):
			return []byte(`{"width":4,"height":3,"screen_width":1600,"screen_height":900,"png":"` + base64.StdEncoding.EncodeToString(shot) + `"}` + "\n"), nil
		case call == "cursor_position":
			return []byte(`{"x":10,"y":20}` + "\n"), nil
		case strings.HasPrefix(call, "left_click 999 "):
			return []byte("boxe-computer: 999,1 is outside the screenshot (1280x720)\n"), exitCode(1)
		}
		action, _, _ := strings.Cut(call, " ")
		return []byte(`{"action":"` + action + `","status":"done"}` + "\n"), nil
	}
	return ran
}

// A screenshot comes out as the PNG it is, over SSH and over the API, or
// as JSON with the VM's name; actions run in the VM with what was asked.
func TestComputerInVM(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.c
	shot := fakePNG(t)
	ran := fakeComputer(t, c, shot)

	if out := c.must("alice", "computer test screenshot"); out != string(shot) {
		t.Errorf("screenshot over SSH: %q", out)
	}
	last := (*ran)[len(*ran)-1]
	if !strings.HasPrefix(last, "command -v boxe-computer >/dev/null || ") || !strings.HasSuffix(last, "; boxe-computer --json screenshot") {
		t.Errorf("command in the VM: %q", last)
	}
	if !c.host.ran("ssh -i "+c.box.hv.HostKeyPath) || !c.host.ranIn("test", "command -v boxe-computer") {
		t.Errorf("not run in test with the host's key: %v", c.host.cmds)
	}
	var j struct {
		Name          string
		Width, Height int
		ScreenWidth   int `json:"screen_width"`
		PNG           string
	}
	c.json("alice", "computer test --json screenshot --full", &j)
	if got, _ := base64.StdEncoding.DecodeString(j.PNG); j.Name != "test" || j.Width != 4 || j.ScreenWidth != 1600 || !bytes.Equal(got, shot) ||
		!strings.HasSuffix((*ran)[len(*ran)-1], "boxe-computer --json screenshot --full") {
		t.Errorf("screenshot --json: %+v, ran %q", j, (*ran)[len(*ran)-1])
	}
	if out := c.must("alice", "computer test cursor_position"); out != "10 20\n" {
		t.Errorf("cursor_position: %q", out)
	}
	var done struct{ Name, Action, Status string }
	c.json("alice", "computer test left-click 10 20 --mod ctrl+shift --json", &done)
	if done != (struct{ Name, Action, Status string }{"test", "left_click", "done"}) {
		t.Errorf("left-click --json: %+v", done)
	}
	if out := c.must("alice", "computer test key ctrl+s Return --repeat 2"); out != "" {
		t.Errorf("key prints %q", out)
	}
	for line, want := range map[string]string{
		"computer test left-click 10 20 --mod ctrl+shift": "left_click 10 20 --mod ctrl+shift",
		"computer test double_click":                      "double_click",
		"computer test scroll down":                       "scroll down",
		"computer test scroll down 5":                     "scroll down 5",
		"computer test scroll up 100 200":                 "scroll up 3 100 200",
		"computer test scroll left 2 100 200 --mod shift": "scroll left 2 100 200 --mod shift",
		"computer test zoom 0 0 640 400":                  "zoom 0 0 640 400",
		"computer test left_click_drag 1 2 3 4":           "left_click_drag 1 2 3 4",
		"computer test mouse_move 007 8":                  "mouse_move 7 8",
		"computer test left_mouse_down":                   "left_mouse_down",
		"computer test hold_key shift 2":                  "hold_key shift 2",
		"computer test wait 1":                            "wait 1",
		"computer test key ctrl+s Return --repeat 2":      "key ctrl+s Return --repeat 2",
	} {
		c.run("alice", line)
		if got := (*ran)[len(*ran)-1]; !strings.HasSuffix(got, "; boxe-computer --json "+want) {
			t.Errorf("%s: ran %q", line, got)
		}
	}

	// The VM's own message, without the program's name, and its status.
	s, _ := c.conn["alice"].NewSession()
	var o, e strings.Builder
	s.Stdout, s.Stderr = &o, &e
	err := s.Run("computer test left_click 999 1")
	var exit interface{ ExitStatus() int }
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 || o.String() != "" || e.String() != "error: 999,1 is outside the screenshot (1280x720)\n" {
		t.Errorf("failing click: %v %q %q", err, o.String(), e.String())
	}
	s.Close()

	// A PNG is not for a terminal.
	s, _ = c.conn["alice"].NewSession()
	s.RequestPty("xterm", 40, 80, gossh.TerminalModes{})
	out, err := s.CombinedOutput("computer test screenshot")
	if err == nil || !strings.Contains(string(out), "redirect it to a file") {
		t.Errorf("screenshot on a terminal: %v %q", err, out)
	}
	s.Close()

	// Asleep, the VM wakes up first.
	c.must("alice", "sleep test")
	if out := c.must("alice", "computer test screenshot"); out != string(shot) || c.box.hv.Sleeping("test") {
		t.Errorf("screenshot of a sleeping VM: sleeping %v", c.box.hv.Sleeping("test"))
	}

	var tok tokenJSON
	c.json("alice", "token new --cmds computer --json", &tok)
	if code, body, h := pt.exec(tok.Token, "computer test screenshot", nil); code != 200 || body != string(shot) ||
		h.Get("Content-Type") != "image/png" || h.Get("X-Exit-Status") != "0" {
		t.Errorf("screenshot over the API: %d %v", code, h)
	}
	if code, body, h := pt.exec(tok.Token, "computer test screenshot --json", nil); code != 200 || !strings.Contains(body, `"name":"test"`) ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("screenshot --json over the API: %d %q %v", code, body, h)
	}
	if code, body, h := pt.exec(tok.Token, "computer test left_click 999 1", nil); code != http.StatusUnprocessableEntity ||
		body != "error: 999,1 is outside the screenshot (1280x720)\n" || h.Get("X-Exit-Status") != "1" {
		t.Errorf("failing click over the API: %d %q %v", code, body, h)
	}
	var other tokenJSON
	c.json("alice", "token new --cmds ls --json", &other)
	if code, _, _ := pt.exec(other.Token, "computer test screenshot", nil); code != http.StatusForbidden {
		t.Errorf("token without computer: %d", code)
	}
}

// Only what is valid for its action reaches the VM; nothing else runs.
func TestComputerChecksItsArguments(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	ran := fakeComputer(t, c, fakePNG(t))
	for _, line := range []string{"computer", "computer test", "computer test mcp", "computer test click 1 2",
		"computer test screenshot --out /tmp/x", "computer test screenshot extra", "computer test zoom 1 2 3",
		"computer test left_click 10", "computer test left_click -1 5", "computer test left_click 1 2 3",
		"computer test left_click 99999999999 1", "computer test left_click 1 2 --mod", "computer test left_click 1 2 --mod 'ctrl;id'",
		"computer test left_click 1 2 --mod -x", "computer test mouse_move 1", "computer test left_mouse_up 1",
		"computer test scroll", "computer test scroll sideways", "computer test scroll down 51", "computer test scroll down 0",
		"computer test scroll down 1 2 3 4", "computer test key", "computer test key ctrl+s;id", "computer test key $(id)",
		"computer test key --full", "computer test key a --repeat 101", "computer test key a --repeat",
		"computer test hold_key a", "computer test hold_key a 61", "computer test wait 61", "computer test wait 0.5",
		"computer test type", "computer test type --json", "computer test cursor_position 1"} {
		if _, _, ok := c.run("alice", line); ok {
			t.Errorf("%s succeeded", line)
		}
	}
	if len(*ran) != 0 {
		t.Errorf("ran in the VM: %q", *ran)
	}
}

// The text to type arrives as typed: apostrophes, words that look like
// flags, a lone dash, unicode; never as shell code.
func TestComputerTypesTextAsIs(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	dir := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + dir + "/args\"\ncat > \"" + dir + "/stdin\"\necho '{\"action\":\"type\",\"status\":\"done\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "boxe-computer"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	c.host.inVM = func(cmd string) ([]byte, error) {
		sh := exec.Command("sh", "-c", cmd)
		sh.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		return sh.CombinedOutput()
	}
	for _, text := range []string{"it's here", `say "hi" $HOME $(id) ` + "`id`" + ` \n`, "hello --json", "--agent do it",
		"naïve → ü 日本", "-", "two  spaces"} {
		if out := c.must("alice", "computer test type "+text); out != "" {
			t.Errorf("type %q printed %q", text, out)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
		if string(args) != "--json\ntype\n-\n" || string(stdin) != text {
			t.Errorf("type %q: args %q, typed %q", text, args, stdin)
		}
	}
	// --json first is the flag; the rest is the text.
	var done struct{ Name, Action, Status string }
	c.json("alice", "computer test type --json it's", &done)
	if stdin, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(stdin) != "it's" || done.Name != "test" || done.Status != "done" {
		t.Errorf("type --json: %q %+v", stdin, done)
	}
}

// computer is for who may use the VM, as its desktop; an admin is not.
// A VM from an older image says it has no boxe computer.
func TestComputerAccess(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.c
	ran := fakeComputer(t, c, fakePNG(t))
	for _, u := range []string{"bob", "dave"} {
		if _, errOut, ok := c.run(u, "computer test cursor_position"); ok || !strings.Contains(errOut, "you have no VM named test") {
			t.Errorf("%s: %v %q", u, ok, errOut)
		}
	}
	if len(*ran) != 0 {
		t.Errorf("ran in the VM: %q", *ran)
	}
	c.must("alice", "access test add bob")
	if out := c.must("bob", "computer test cursor_position"); out != "10 20\n" {
		t.Errorf("bob with access: %q", out)
	}

	c.host.inVM = func(cmd string) ([]byte, error) {
		return []byte("this VM has no boxe computer: its image is older, make a new VM\n"), exitCode(3)
	}
	s, _ := c.conn["alice"].NewSession()
	var e strings.Builder
	s.Stderr = &e
	err := s.Run("computer test screenshot")
	var exit interface{ ExitStatus() int }
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || !strings.Contains(e.String(), "this VM has no boxe computer") {
		t.Errorf("older image: %v %q", err, e.String())
	}
	s.Close()
	var j map[string]string
	out, _, _ := c.run("alice", "computer test screenshot --json")
	if json.Unmarshal([]byte(out), &j); !strings.Contains(j["error"], "no boxe computer") {
		t.Errorf("older image, --json: %q", out)
	}
}

// The prompt completes the VM, then the action.
func TestPromptCompletesComputer(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	for line, want := range map[string]string{"computer te": "computer test ", "computer test scre": "computer test screenshot ",
		"computer test left_click_": "computer test left_click_drag "} {
		if got, _, ok := c.box.complete("alice", line, len(line)); !ok || got != want {
			t.Errorf("%q: %q %v", line, got, ok)
		}
	}
}

// Whatever boxe-computer in the VM prints, boxed answers with an error, and
// keeps running.
func TestComputerOddAnswers(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	for _, answer := range []string{"null", "[]", `"x"`, "42", "{"} {
		c.host.inVM = func(string) ([]byte, error) { return []byte(answer + "\n"), nil }
		for _, line := range []string{"computer test --json cursor_position", "computer test cursor_position", "computer test --json screenshot"} {
			if _, _, ok := c.run("alice", line); ok {
				t.Errorf("%s answering %s succeeded", line, answer)
			}
		}
	}
}
