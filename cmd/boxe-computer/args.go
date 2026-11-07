package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// usageError is a mistake in how the command was called: exit status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

const (
	maxScroll   = 50
	maxRepeat   = 100
	maxSeconds  = 60
	maxTypeText = 8 << 10
)

// actions are the computer-use names of Anthropic's tool, which models
// already know; the CLI also takes them with hyphens.
var actions = []string{
	"screenshot", "zoom",
	"left_click", "right_click", "middle_click", "double_click", "triple_click",
	"left_click_drag", "mouse_move", "left_mouse_down", "left_mouse_up",
	"cursor_position", "scroll", "type", "key", "hold_key", "wait",
}

func canonicalAction(s string) (string, bool) {
	s = strings.ReplaceAll(s, "-", "_")
	for _, a := range actions {
		if a == s {
			return a, true
		}
	}
	return "", false
}

type point struct{ x, y int }

// request is one validated action, from the command line or from MCP.
// Coordinates are in screenshot space.
type request struct {
	action  string
	at      *point // clicks, mouse_move, scroll, drag's end
	from    *point // drag's start
	region  [4]int // zoom
	mods    []string
	dir     string
	amount  int
	text    string
	keys    []string
	repeat  int
	seconds float64
	full    bool
	out     string
}

func isClick(a string) bool {
	switch a {
	case "left_click", "right_click", "middle_click", "double_click", "triple_click":
		return true
	}
	return false
}

// parseArgs parses "<action> [args]" (after the global flags). stdin is read
// only by "type -".
func parseArgs(args []string, stdin io.Reader) (*request, error) {
	if len(args) == 0 {
		return nil, usagef("which action? (boxe-computer help)")
	}
	action, ok := canonicalAction(args[0])
	if !ok {
		return nil, usagef("unknown action %q (boxe-computer help)", args[0])
	}
	r := &request{action: action, repeat: 1}
	if action == "type" {
		// Everything is text: no flags, so "--" or "-x" can be typed.
		text := strings.Join(args[1:], " ")
		if len(args) == 2 && args[1] == "-" {
			b, err := io.ReadAll(io.LimitReader(stdin, maxTypeText+1))
			if err != nil {
				return nil, err
			}
			if len(b) > maxTypeText {
				return nil, usagef("the text is longer than 8 KiB")
			}
			text = string(b)
		}
		if text == "" {
			return nil, usagef("type needs the text to type")
		}
		r.text = text
		return r, nil
	}

	flags := map[string]bool{}
	switch {
	case action == "screenshot":
		flags["full"], flags["out"] = true, true
	case action == "zoom":
		flags["out"] = true
	case isClick(action), action == "scroll":
		flags["mod"] = true
	case action == "key":
		flags["repeat"] = true
	}
	var pos []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		if name == "json" && !hasVal {
			continue // the global flag, also accepted here
		}
		if !flags[name] {
			return nil, usagef("%s does not take --%s", action, name)
		}
		if name == "full" {
			if hasVal {
				return nil, usagef("--full takes no value")
			}
			r.full = true
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, usagef("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		var err error
		switch name {
		case "out":
			if val == "" {
				return nil, usagef("--out needs a file name")
			}
			r.out = val
		case "mod":
			r.mods, err = parseMods(val)
		case "repeat":
			r.repeat, err = parseRange("--repeat", val, 1, maxRepeat)
		}
		if err != nil {
			return nil, err
		}
	}

	var err error
	switch {
	case action == "screenshot", action == "left_mouse_down", action == "left_mouse_up", action == "cursor_position":
		err = wantArgs(action, pos, 0)
	case action == "zoom":
		if err = wantArgs(action+" X0 Y0 X1 Y1", pos, 4); err == nil {
			for i := range 4 {
				if r.region[i], err = parseInt(pos[i]); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = checkRegion(r.region)
		}
	case isClick(action):
		if len(pos) != 0 {
			r.at, err = parsePoint(action+" [X Y]", pos)
		}
	case action == "mouse_move":
		r.at, err = parsePoint(action+" X Y", pos)
	case action == "left_click_drag":
		if err = wantArgs(action+" X0 Y0 X1 Y1", pos, 4); err == nil {
			if r.from, err = parsePoint("", pos[:2]); err == nil {
				r.at, err = parsePoint("", pos[2:])
			}
		}
	case action == "scroll":
		err = parseScroll(r, pos)
	case action == "key":
		if len(pos) == 0 {
			return nil, usagef("key needs a key, such as Return or ctrl+s")
		}
		r.keys, err = parseKeys(strings.Join(pos, " "))
	case action == "hold_key":
		if err = wantArgs("hold_key COMBO SECONDS", pos, 2); err == nil {
			if r.keys, err = parseKeys(pos[0]); err == nil {
				r.seconds, err = parseSeconds(pos[1])
			}
		}
	case action == "wait":
		if err = wantArgs("wait SECONDS", pos, 1); err == nil {
			r.seconds, err = parseSeconds(pos[0])
		}
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func wantArgs(form string, pos []string, n int) error {
	if len(pos) != n {
		if n == 0 {
			return usagef("%s takes no arguments", form)
		}
		return usagef("usage: %s", form)
	}
	return nil
}

func parseScroll(r *request, pos []string) error {
	if len(pos) == 0 {
		return usagef("usage: scroll up|down|left|right [N] [X Y]")
	}
	r.dir = strings.ToLower(pos[0])
	if err := checkDir(r.dir); err != nil {
		return err
	}
	r.amount = 3
	rest := pos[1:]
	var err error
	switch len(rest) {
	case 0:
	case 1, 3:
		if r.amount, err = parseRange("the scroll amount", rest[0], 1, maxScroll); err != nil {
			return err
		}
		if len(rest) == 3 {
			r.at, err = parsePoint("", rest[1:])
		}
	case 2:
		r.at, err = parsePoint("", rest)
	default:
		return usagef("usage: scroll up|down|left|right [N] [X Y]")
	}
	return err
}

func checkDir(d string) error {
	switch d {
	case "up", "down", "left", "right":
		return nil
	}
	return usagef("the scroll direction is up, down, left or right, not %q", d)
}

func parsePoint(form string, pos []string) (*point, error) {
	if len(pos) != 2 {
		return nil, usagef("usage: %s", form)
	}
	x, err := parseInt(pos[0])
	if err != nil {
		return nil, err
	}
	y, err := parseInt(pos[1])
	if err != nil {
		return nil, err
	}
	return &point{x, y}, nil
}

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, usagef("%q is not a whole number", s)
	}
	return n, nil
}

func parseRange(what, s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, usagef("%s is a whole number from %d to %d, not %q", what, lo, hi, s)
	}
	return n, nil
}

func parseSeconds(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, usagef("%q is not a number of seconds", s)
	}
	return f, checkSeconds(f)
}

func checkSeconds(f float64) error {
	if math.IsNaN(f) || f < 0 || f > maxSeconds {
		return usagef("seconds go from 0 to %d", maxSeconds)
	}
	return nil
}

func checkRegion(g [4]int) error {
	if g[0] >= g[2] || g[1] >= g[3] {
		return usagef("the zoom region is X0 Y0 X1 Y1 with X0 < X1 and Y0 < Y1")
	}
	return nil
}

// keyAliases maps the names models use to xdotool's keysyms, matched
// without case on each "+"-separated part of a combo.
var keyAliases = map[string]string{
	"enter": "Return", "return": "Return",
	"esc": "Escape", "escape": "Escape",
	"backspace": "BackSpace",
	"del":       "Delete", "delete": "Delete",
	"tab":   "Tab",
	"space": "space",
	"up":    "Up", "arrowup": "Up",
	"down": "Down", "arrowdown": "Down",
	"left": "Left", "arrowleft": "Left",
	"right": "Right", "arrowright": "Right",
	"pageup": "Page_Up", "pagedown": "Page_Down",
	"home": "Home", "end": "End",
	"cmd": "super", "command": "super", "meta": "super", "win": "super", "super": "super",
	"option": "alt", "alt": "alt",
	"control": "ctrl", "ctrl": "ctrl",
	"shift":    "shift",
	"capslock": "Caps_Lock",
}

func mapCombo(combo string) (string, error) {
	parts := strings.Split(combo, "+")
	for i, p := range parts {
		if p == "" {
			return "", usagef("%q is not a key combination", combo)
		}
		if len([]rune(p)) == 1 {
			continue // a single letter keeps its case
		}
		if k, ok := keyAliases[strings.ToLower(p)]; ok {
			parts[i] = k
		}
	}
	return strings.Join(parts, "+"), nil
}

// parseKeys takes one or more combos separated by spaces.
func parseKeys(s string) ([]string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, usagef("which key? such as Return or ctrl+s")
	}
	for i, f := range fields {
		k, err := mapCombo(f)
		if err != nil {
			return nil, err
		}
		fields[i] = k
	}
	return fields, nil
}

// parseMods takes modifiers for clicks and scrolls, such as "ctrl+shift".
func parseMods(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	k, err := mapCombo(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	mods := strings.Split(k, "+")
	for _, m := range mods {
		switch m {
		case "shift", "ctrl", "alt", "super":
		default:
			return nil, usagef("the modifiers are shift, ctrl, alt and super, not %q", m)
		}
	}
	return mods, nil
}
