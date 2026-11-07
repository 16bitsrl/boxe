package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gliderlabs/ssh"
	"golang.org/x/term"
)

// `computer <name> <action> [args]` drives the desktop of a VM with
// boxe-computer, the program in the image that agents in the VM use too:
// the same actions, with the names of Anthropic's computer use tool. boxed
// checks every argument and writes the VM's command line from them only.

// computerActions are boxe-computer's actions, with their arguments as
// the usage shows them; mcp is for the VM's agents only.
var computerActions = []struct{ name, args string }{
	{"screenshot", "[--full]"},
	{"zoom", "X0 Y0 X1 Y1"},
	{"left_click", "[X Y] [--mod MODS]"},
	{"right_click", "[X Y] [--mod MODS]"},
	{"middle_click", "[X Y] [--mod MODS]"},
	{"double_click", "[X Y] [--mod MODS]"},
	{"triple_click", "[X Y] [--mod MODS]"},
	{"left_click_drag", "X0 Y0 X1 Y1"},
	{"mouse_move", "X Y"},
	{"left_mouse_down", ""},
	{"left_mouse_up", ""},
	{"cursor_position", ""},
	{"scroll", "up|down|left|right [N] [X Y] [--mod MODS]"},
	{"type", "TEXT"},
	{"key", "COMBO... [--repeat N]"},
	{"hold_key", "COMBO SECONDS"},
	{"wait", "SECONDS"},
}

// computerGuard is what a VM from an image without boxe-computer answers.
const computerGuard = "command -v boxe-computer >/dev/null || { echo 'this VM has no boxe computer: its image is older, make a new VM'; exit 3; }; "

// maxCoord is the largest X screen; and so the largest coordinate.
const maxCoord = 32767

var validCombo = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9_+-]*$`)

// computerCmd is a computer line, checked.
type computerCmd struct {
	name, action string
	args         []string // for the VM's command line, each checked
	text         string   // what type types
	json         bool
}

// parseComputer reads `computer <name> <action> [args]` from the line as
// typed: the text of type is the rest of it, quotes and all. --json is
// the flag anywhere but in that text, where it is only at its start.
func parseComputer(box, line string) (computerCmd, error) {
	var c computerCmd
	usage := func(action string) error {
		if action == "" {
			names := []string{}
			for _, a := range computerActions {
				names = append(names, a.name)
			}
			return fmt.Errorf("usage: ssh %s computer <name> <action> [args], the actions: %s", box, strings.Join(names, ", "))
		}
		i := slices.IndexFunc(computerActions, func(a struct{ name, args string }) bool { return a.name == action })
		return fmt.Errorf("usage: ssh %s computer <name> %s %s", box, action, computerActions[i].args)
	}
	s := strings.TrimLeft(line, " \t\r\n")
	// next takes the next word and the one blank after it.
	next := func() string {
		s = strings.TrimLeft(s, " \t\r\n")
		i := strings.IndexAny(s, " \t\r\n")
		if i < 0 {
			w := s
			s = ""
			return w
		}
		w := s[:i]
		s = s[i+1:]
		return w
	}
	word := func() string {
		for {
			if w := next(); w != "--json" {
				return w
			}
			c.json = true
		}
	}
	if word() != "computer" {
		return c, usage("")
	}
	c.name, c.action = word(), strings.ReplaceAll(word(), "-", "_")
	if c.name == "" || c.action == "" {
		return c, usage("")
	}
	if !slices.ContainsFunc(computerActions, func(a struct{ name, args string }) bool { return a.name == c.action }) {
		return c, fmt.Errorf("unknown action %s: %w", c.action, usage(""))
	}
	if c.action == "type" {
		if rest, ok := strings.CutPrefix(s, "--json"); ok && (rest == "" || rest[0] == ' ') {
			c.json, s = true, strings.TrimPrefix(rest, " ")
		}
		c.text = s
		if c.text == "" {
			return c, usage(c.action)
		}
		// boxe-computer types 12 ms a character and pauses around accents:
		// 8K end within execTimeout (TestLongestTextEndsInTime there).
		if len(c.text) > 8<<10 {
			return c, errors.New("the text is longer than 8K")
		}
		return c, nil
	}
	words, asJSON := cutFlag(strings.Fields(s), "--json")
	c.json = c.json || asJSON
	args, ok := computerArgs(c.action, words)
	if !ok {
		return c, usage(c.action)
	}
	c.args = args
	return c, nil
}

// computerArgs checks the arguments of action and writes them as
// boxe-computer reads them; false if they are not valid for it.
func computerArgs(action string, words []string) ([]string, bool) {
	// Flags with a value, and the rest in order.
	var pos []string
	flags := map[string]string{}
	for i := 0; i < len(words); i++ {
		switch w := words[i]; w {
		case "--mod", "--repeat":
			if i+1 == len(words) {
				return nil, false
			}
			flags[w] = words[i+1]
			i++
		default:
			pos = append(pos, w)
		}
	}
	num := func(s string, lo, hi int) bool {
		n, err := strconv.Atoi(s)
		return err == nil && s[0] != '-' && s[0] != '+' && n >= lo && n <= hi
	}
	ints := func(ss []string, lo, hi int) ([]string, bool) {
		out := []string{}
		for _, s := range ss {
			if !num(s, lo, hi) {
				return nil, false
			}
			n, _ := strconv.Atoi(s)
			out = append(out, strconv.Itoa(n))
		}
		return out, true
	}
	allow := func(names ...string) bool {
		for f, v := range flags {
			if !slices.Contains(names, f) || !validCombo.MatchString(v) {
				return false
			}
		}
		return true
	}
	withMod := func(args []string) []string {
		if m, ok := flags["--mod"]; ok {
			return append(args, "--mod", m)
		}
		return args
	}
	switch action {
	case "screenshot":
		if len(flags) > 0 || len(pos) > 1 || len(pos) == 1 && pos[0] != "--full" {
			return nil, false
		}
		return pos, true
	case "zoom", "left_click_drag", "mouse_move", "left_mouse_down", "left_mouse_up", "cursor_position":
		want := map[string]int{"zoom": 4, "left_click_drag": 4, "mouse_move": 2}[action]
		xy, ok := ints(pos, 0, maxCoord)
		return xy, ok && len(flags) == 0 && len(pos) == want
	case "left_click", "right_click", "middle_click", "double_click", "triple_click":
		xy, ok := ints(pos, 0, maxCoord)
		return withMod(xy), ok && allow("--mod") && (len(pos) == 0 || len(pos) == 2)
	case "scroll":
		if len(pos) == 0 || !slices.Contains([]string{"up", "down", "left", "right"}, pos[0]) || len(pos) > 4 || !allow("--mod") {
			return nil, false
		}
		n, ok := ints(pos[1:], 0, maxCoord)
		switch {
		case !ok:
			return nil, false
		case len(n) == 2: // X Y: the default number of notches, said
			n = append([]string{"3"}, n...)
		case len(n) > 0 && !num(n[0], 1, 50):
			return nil, false
		}
		return withMod(append([]string{pos[0]}, n...)), true
	case "key":
		if len(pos) == 0 || !allow("--repeat") {
			return nil, false
		}
		for _, k := range pos {
			if !validCombo.MatchString(k) {
				return nil, false
			}
		}
		if r, ok := flags["--repeat"]; ok {
			if !num(r, 1, 100) {
				return nil, false
			}
			return append(pos, "--repeat", r), true
		}
		return pos, true
	case "hold_key":
		ok := len(flags) == 0 && len(pos) == 2 && validCombo.MatchString(pos[0]) && num(pos[1], 0, 60)
		return pos, ok
	case "wait":
		ok := len(flags) == 0 && len(pos) == 1 && num(pos[0], 0, 60)
		return pos, ok
	}
	return nil, false
}

// vmCommand is the command line for the VM: boxe-computer with the
// checked arguments, the text to type single-quoted on its stdin.
func (c computerCmd) vmCommand() string {
	if c.action == "type" {
		return computerGuard + "printf '%s' '" + strings.ReplaceAll(c.text, "'", `'\''`) + "' | boxe-computer --json type -"
	}
	return computerGuard + strings.Join(append([]string{"boxe-computer --json", c.action}, c.args...), " ")
}

// computerError is boxe-computer failing in the VM: its message, and its
// exit status for ssh and the API.
type computerError struct {
	msg    string
	status exitStatus
}

func (e computerError) Error() string { return e.msg }
func (e computerError) Unwrap() error { return e.status }

// isTerminal says whether w shows what it gets on a terminal: an SSH
// session with a pty, or the prompt.
func isTerminal(w io.Writer) bool {
	switch w := w.(type) {
	case ssh.Session:
		_, _, pty := w.Pty()
		return pty
	case *term.Terminal:
		return true
	}
	return false
}

// cmdComputer is `computer <name> <action> [args]`, for who may use the
// VM, as its desktop: a screenshot comes as a PNG, or base64 with --json.
func (b *Box) cmdComputer(w io.Writer, user, line string) error {
	c, err := parseComputer(b.name, line)
	if err == nil {
		err = b.computer(w, user, c)
	}
	if err != nil && c.json {
		output{w, true}.print(map[string]string{"error": err.Error()}, "")
	}
	return err
}

func (b *Box) computer(w io.Writer, user string, c computerCmd) error {
	isImage := c.action == "screenshot" || c.action == "zoom"
	if isImage && !c.json && isTerminal(w) {
		return fmt.Errorf("the screenshot is a PNG: redirect it to a file (> screen.png) or add --json")
	}
	v, hostKey, err := b.reach(user, c.name)
	if err != nil {
		return err
	}
	b.busy(v.Name, 1)
	defer b.busy(v.Name, -1)
	out, err := b.hv.inVMWithKey(v, hostKey, execTimeout, c.vmCommand())
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 255: // ssh's own failure
		log.Printf("%s: computer in %s: %s", user, v.Name, bytes.TrimSpace(out))
		return hostErrorf("could not reach %s", v.Name)
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		msg := strings.TrimPrefix(lines[len(lines)-1], "boxe-computer: ")
		if msg == "" {
			msg = exitStatus(exit.ExitCode()).Error()
		}
		return computerError{msg, exitStatus(exit.ExitCode())}
	default:
		log.Printf("%s: computer in %s: %v", user, v.Name, errors.Unwrap(err))
		return hostErrorf("could not drive the desktop of %s, or it took more than %s", v.Name, execTimeout)
	}
	// Its answer is one JSON line; ssh may have printed before it.
	var answer map[string]json.RawMessage
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := []byte(lines[len(lines)-1])
	if err := json.Unmarshal(last, &answer); err != nil || answer == nil {
		log.Printf("%s: computer in %s: %q", user, v.Name, out)
		return hostErrorf("could not read what boxe-computer in %s answered", v.Name)
	}
	if c.json {
		answer["name"], _ = json.Marshal(v.Name)
		output{w, true}.print(answer, "")
		return nil
	}
	switch {
	case isImage:
		var b64 string
		json.Unmarshal(answer["png"], &b64)
		png, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(png) == 0 {
			return hostErrorf("could not read the screenshot of %s", v.Name)
		}
		_, err = w.Write(png)
		return err
	case c.action == "cursor_position":
		var p struct{ X, Y int }
		json.Unmarshal(last, &p)
		fmt.Fprintf(w, "%d %d\n", p.X, p.Y)
	}
	return nil
}
