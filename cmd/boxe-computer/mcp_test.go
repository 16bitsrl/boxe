package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mcpSession talks newline-delimited JSON-RPC to serveMCP, as an agent does.
type mcpSession struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
	id  int
}

func startMCP(t *testing.T, c *computer) *mcpSession {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMCP(c, inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the server did not stop at the end of stdin")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 16<<20)
	return &mcpSession{t: t, in: inW, out: sc}
}

func (s *mcpSession) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *mcpSession) call(method string, params any) json.RawMessage {
	s.t.Helper()
	s.id++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	for s.out.Scan() {
		var m struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(s.out.Bytes(), &m); err != nil {
			s.t.Fatalf("%s: %v", s.out.Text(), err)
		}
		if m.ID == nil || *m.ID != s.id {
			continue // a notification
		}
		if m.Error != nil {
			s.t.Fatalf("%s: %s", method, m.Error)
		}
		return m.Result
	}
	s.t.Fatalf("%s: no answer: %v", method, s.out.Err())
	return nil
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
	} `json:"content"`
}

func (s *mcpSession) tool(args string) toolResult {
	s.t.Helper()
	var r toolResult
	if err := json.Unmarshal(s.call("tools/call", map[string]any{"name": "computer", "arguments": json.RawMessage(args)}), &r); err != nil {
		s.t.Fatal(err)
	}
	return r
}

func TestMCP(t *testing.T) {
	f := newFake(t, 1600, 900)
	s := startMCP(t, f.computer)

	var init struct {
		ProtocolVersion string                `json:"protocolVersion"`
		ServerInfo      struct{ Name string } `json:"serverInfo"`
		Capabilities    map[string]any        `json:"capabilities"`
	}
	json.Unmarshal(s.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	}), &init)
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "boxe-computer" || init.Capabilities["tools"] == nil {
		t.Fatalf("initialize: %+v", init)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.call("ping", map[string]any{})

	var list struct {
		Tools []struct {
			Name        string
			Description string
			InputSchema struct {
				Type       string
				Properties map[string]struct {
					Enum []string
				}
				Required []string
			}
		}
	}
	json.Unmarshal(s.call("tools/list", map[string]any{}), &list)
	if len(list.Tools) != 1 || list.Tools[0].Name != "computer" {
		t.Fatalf("tools: %+v", list.Tools)
	}
	tool := list.Tools[0]
	if !reflect.DeepEqual(tool.InputSchema.Properties["action"].Enum, actions) || !reflect.DeepEqual(tool.InputSchema.Required, []string{"action"}) {
		t.Errorf("schema: %+v", tool.InputSchema)
	}
	for _, p := range []string{"coordinate", "start_coordinate", "text", "scroll_direction", "scroll_amount", "duration", "region", "repeat"} {
		if _, ok := tool.InputSchema.Properties[p]; !ok {
			t.Errorf("schema has no %s", p)
		}
	}
	for _, w := range []string{"DISPLAY=:1", "desktop page", "screenshot first", "last screenshot", "starts by itself"} {
		if !strings.Contains(tool.Description, w) {
			t.Errorf("description lacks %q", w)
		}
	}

	checkImage := func(r toolResult, w, h int) {
		t.Helper()
		if r.IsError || len(r.Content) != 2 || r.Content[0].Type != "image" || r.Content[0].MIMEType != "image/png" || r.Content[1].Type != "text" {
			t.Fatalf("got %+v", r)
		}
		if want := fmt.Sprintf("screen 1600x900, image %dx%d", w, h); r.Content[1].Text != want {
			t.Errorf("text %q, want %q", r.Content[1].Text, want)
		}
		b, err := base64.StdEncoding.DecodeString(r.Content[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodePNG(t, b).Bounds(); got.Dx() != w || got.Dy() != h {
			t.Errorf("image is %v", got)
		}
	}
	checkImage(s.tool(`{"action":"screenshot"}`), 1280, 720)
	checkImage(s.tool(`{"action":"zoom","region":[0,0,640,360]}`), 800, 450)

	// Actions run, wait, and answer with a new screenshot.
	f.calls, f.slept = nil, nil
	checkImage(s.tool(`{"action":"left_click","coordinate":[640,360],"text":"shift"}`), 1280, 720)
	checkImage(s.tool(`{"action":"left_click_drag","start_coordinate":[0,0],"coordinate":[100,1]}`), 1280, 720)
	checkImage(s.tool(`{"action":"scroll","scroll_direction":"down","scroll_amount":2,"coordinate":[8,8]}`), 1280, 720)
	checkImage(s.tool(`{"action":"type","text":"hello world"}`), 1280, 720)
	checkImage(s.tool(`{"action":"key","text":"enter","repeat":2}`), 1280, 720)
	checkImage(s.tool(`{"action":"hold_key","text":"ctrl","duration":1}`), 1280, 720)
	checkImage(s.tool(`{"action":"wait","duration":2}`), 1280, 720)
	want := argv(
		"mousemove --sync 800 450", "keydown shift", "click 1", "keyup shift",
		"mousemove --sync 0 0", "mousedown 1", "mousemove --sync 125 1", "mouseup 1",
		"mousemove --sync 10 10", "click --repeat 2 --delay 30 5",
		"type --delay 12 -- hello", "key --repeat 2 --delay 50 -- Return",
		"keydown -- ctrl", "keyup -- ctrl")
	want[10] = []string{"type", "--delay", "12", "--", "hello world"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("xdotool:\n got %q\nwant %q", f.calls, want)
	}
	wantSleep := []time.Duration{settle, settle, settle, settle, settle, time.Second, settle, 2 * time.Second}
	if !reflect.DeepEqual(f.slept, wantSleep) {
		t.Errorf("slept %v, want %v", f.slept, wantSleep)
	}

	f.scr.px, f.scr.py = 800, 450
	if r := s.tool(`{"action":"cursor_position"}`); r.IsError || len(r.Content) != 1 || r.Content[0].Text != "640 360" {
		t.Errorf("cursor_position: %+v", r)
	}

	for args, msg := range map[string]string{
		`{"action":"fly"}`:                                `unknown action "fly"`,
		`{"action":"mouse_move"}`:                         "mouse_move needs coordinate",
		`{"action":"left_click","coordinate":[5000,1]}`:   "5000,1 is outside the screenshot (1280x720)",
		`{"action":"left_click","coordinate":[1.5,1]}`:    "whole numbers",
		`{"action":"left_click","coordinate":[1]}`:        "coordinate needs 2 numbers, not 1",
		`{"action":"scroll","scroll_direction":"in"}`:     "up, down, left or right",
		`{"action":"key","text":""}`:                      "key needs text",
		`{"action":"key","text":"a","repeat":0}`:          "repeat is a whole number from 1 to 100",
		`{"action":"wait","duration":61}`:                 "seconds go from 0 to 60",
		`{"action":"zoom","region":[10,0,5,5]}`:           "X0 < X1",
		`{"action":"screenshot","coordinates":[1,1]}`:     "bad arguments",
		`{"action":"left_click","text":"ctrl+q"}`:         "modifiers are shift, ctrl, alt and super",
		`{"action":"left_click_drag","coordinate":[1,1]}`: "needs start_coordinate and coordinate",
		`{"action":"type"}`:                               "type needs text",
	} {
		r := s.tool(args)
		if !r.IsError || len(r.Content) != 1 || !strings.Contains(r.Content[0].Text, msg) {
			t.Errorf("%s: got %+v, want an error with %q", args, r, msg)
		}
	}

	// The screen changed size: the action refuses, nothing is clicked.
	f.scr.w, f.scr.h = 1710, 1128
	f.calls = nil
	if r := s.tool(`{"action":"left_click","coordinate":[1,1]}`); !r.IsError || !strings.Contains(r.Content[0].Text, "take a new screenshot") || f.calls != nil {
		t.Errorf("size changed: %+v, %q", r, f.calls)
	}
	r := s.tool(`{"action":"screenshot"}`)
	if r.IsError || r.Content[1].Text != "screen 1710x1128, image 1213x800" {
		t.Errorf("after resize: %+v", r.Content[1].Text)
	}
}
