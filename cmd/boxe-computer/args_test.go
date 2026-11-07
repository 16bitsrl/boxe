package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args []string
		want request
	}{
		{[]string{"screenshot"}, request{action: "screenshot", repeat: 1}},
		{[]string{"screenshot", "--full", "--out", "s.png"}, request{action: "screenshot", repeat: 1, full: true, out: "s.png"}},
		{[]string{"screenshot", "--out=s.png", "--json"}, request{action: "screenshot", repeat: 1, out: "s.png"}},
		{[]string{"zoom", "0", "10", "100", "200"}, request{action: "zoom", repeat: 1, region: [4]int{0, 10, 100, 200}}},
		{[]string{"left_click"}, request{action: "left_click", repeat: 1}},
		{[]string{"left-click", "5", "6"}, request{action: "left_click", repeat: 1, at: &point{5, 6}}},
		{[]string{"double_click", "5", "6", "--mod", "Control+Shift"}, request{action: "double_click", repeat: 1, at: &point{5, 6}, mods: []string{"ctrl", "shift"}}},
		{[]string{"triple-click", "--mod=cmd"}, request{action: "triple_click", repeat: 1, mods: []string{"super"}}},
		{[]string{"right_click", "1", "2"}, request{action: "right_click", repeat: 1, at: &point{1, 2}}},
		{[]string{"middle_click"}, request{action: "middle_click", repeat: 1}},
		{[]string{"left_click_drag", "1", "2", "3", "4"}, request{action: "left_click_drag", repeat: 1, from: &point{1, 2}, at: &point{3, 4}}},
		{[]string{"mouse-move", "7", "8"}, request{action: "mouse_move", repeat: 1, at: &point{7, 8}}},
		{[]string{"left_mouse_down"}, request{action: "left_mouse_down", repeat: 1}},
		{[]string{"left-mouse-up"}, request{action: "left_mouse_up", repeat: 1}},
		{[]string{"cursor_position"}, request{action: "cursor_position", repeat: 1}},
		{[]string{"scroll", "down"}, request{action: "scroll", repeat: 1, dir: "down", amount: 3}},
		{[]string{"scroll", "Up", "10"}, request{action: "scroll", repeat: 1, dir: "up", amount: 10}},
		{[]string{"scroll", "left", "4", "5"}, request{action: "scroll", repeat: 1, dir: "left", amount: 3, at: &point{4, 5}}},
		{[]string{"scroll", "right", "2", "4", "5", "--mod", "shift"}, request{action: "scroll", repeat: 1, dir: "right", amount: 2, at: &point{4, 5}, mods: []string{"shift"}}},
		{[]string{"type", "hello", "--json", "-x"}, request{action: "type", repeat: 1, text: "hello --json -x"}},
		{[]string{"type", "a  b"}, request{action: "type", repeat: 1, text: "a  b"}},
		{[]string{"key", "ctrl+s"}, request{action: "key", repeat: 1, keys: []string{"ctrl+s"}}},
		{[]string{"key", "enter", "ctrl+A", "--repeat", "3"}, request{action: "key", repeat: 3, keys: []string{"Return", "ctrl+A"}}},
		{[]string{"hold_key", "shift", "1.5"}, request{action: "hold_key", repeat: 1, keys: []string{"shift"}, seconds: 1.5}},
		{[]string{"wait", "0"}, request{action: "wait", repeat: 1}},
		{[]string{"wait", "60"}, request{action: "wait", repeat: 1, seconds: 60}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, nil)
		if err != nil {
			t.Errorf("%q: %v", tt.args, err)
			continue
		}
		if !reflect.DeepEqual(*got, tt.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", tt.args, *got, tt.want)
		}
	}
}

func TestParseArgsErrors(t *testing.T) {
	tests := [][]string{
		{},
		{"fly"},
		{"mcp"}, // not an action: run handles it
		{"screenshot", "1"},
		{"screenshot", "--mod", "ctrl"},
		{"screenshot", "--full=yes"},
		{"screenshot", "--out"},
		{"zoom", "1", "2", "3"},
		{"zoom", "10", "0", "5", "20"},
		{"zoom", "0", "0", "10", "10", "--full"},
		{"left_click", "5"},
		{"left_click", "a", "b"},
		{"left_click", "1", "2", "3"},
		{"left_click", "--mod", "ctrl+x"},
		{"left_click", "--mod", "ctrl++"},
		{"mouse_move"},
		{"left_click_drag", "1", "2", "3"},
		{"left_mouse_down", "1", "2"},
		{"cursor_position", "--json", "x"},
		{"scroll"},
		{"scroll", "sideways"},
		{"scroll", "up", "0"},
		{"scroll", "up", "51"},
		{"scroll", "up", "1", "2", "3", "4"},
		{"type"},
		{"key"},
		{"key", "ctrl+s", "--repeat", "0"},
		{"key", "ctrl+s", "--repeat", "101"},
		{"key", "a+"},
		{"hold_key", "shift"},
		{"hold_key", "shift", "61"},
		{"hold_key", "shift", "-1"},
		{"wait"},
		{"wait", "NaN"},
		{"wait", "abc"},
	}
	for _, args := range tests {
		_, err := parseArgs(args, nil)
		if !errors.As(err, new(usageError)) {
			t.Errorf("%q: got %v, want a usage error", args, err)
		}
	}
}

func TestTypeStdin(t *testing.T) {
	r, err := parseArgs([]string{"type", "-"}, strings.NewReader("line one\nline two\n"))
	if err != nil || r.text != "line one\nline two\n" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := parseArgs([]string{"type", "-"}, strings.NewReader("")); err == nil {
		t.Error("empty stdin: no error")
	}
	if _, err := parseArgs([]string{"type", "-"}, strings.NewReader(strings.Repeat("a", maxTypeText+1))); err == nil {
		t.Error("too long: no error")
	}
	if r, _ := parseArgs([]string{"type", "-", "-"}, nil); r.text != "- -" {
		t.Errorf("type - -: got %q", r.text)
	}
}

func TestMapCombo(t *testing.T) {
	tests := map[string]string{
		"Return":         "Return",
		"enter":          "Return",
		"ENTER":          "Return",
		"esc":            "Escape",
		"Escape":         "Escape",
		"backspace":      "BackSpace",
		"del":            "Delete",
		"tab":            "Tab",
		"Space":          "space",
		"arrowup":        "Up",
		"down":           "Down",
		"ArrowLeft":      "Left",
		"right":          "Right",
		"pageup":         "Page_Up",
		"PageDown":       "Page_Down",
		"home":           "Home",
		"end":            "End",
		"cmd+c":          "super+c",
		"Meta+Shift+P":   "super+shift+P",
		"win":            "super",
		"option+Tab":     "alt+Tab",
		"Control+a":      "ctrl+a",
		"capslock":       "Caps_Lock",
		"ctrl+s":         "ctrl+s",
		"F5":             "F5",
		"Page_Down":      "Page_Down",
		"KP_Enter":       "KP_Enter",
		"A":              "A",
		"alt+Tab":        "alt+Tab",
		"ctrl+shift+Esc": "ctrl+shift+Escape",
	}
	for in, want := range tests {
		if got, err := mapCombo(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}
