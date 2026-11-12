package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// browse-mcp hands the agent's messages to browse mcp as they are, and
// starts the desktop before each tool call, not before the handshake: an
// agent that never uses the browser does not start the desktop.
func TestBrowseMCPStartsTheDesktopOnlyForToolCalls(t *testing.T) {
	// A browse that echoes what it gets, as browse mcp would answer it.
	fake := filepath.Join(t.TempDir(), "browse")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1\" = mcp ] || exit 2\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := browseBin
	t.Cleanup(func() { browseBin = old })
	browseBin = fake

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"execute","arguments":{"code":"return 1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"status"}}`,
	}, "\n") + "\n"
	var out, log strings.Builder
	ensured := 0
	ensure := func() error {
		ensured++
		if ensured == 2 {
			return errors.New("Chrome did not connect")
		}
		return nil
	}
	if err := serveBrowseMCP(strings.NewReader(in), &out, &log, ensure); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Fatalf("browse got:\n%s", out.String())
	}
	if ensured != 2 {
		t.Fatalf("desktop started %d times, want once per tool call", ensured)
	}
	// A failure to start goes to the log, and the call still reaches browse,
	// which tells the agent what is wrong.
	if !strings.Contains(log.String(), "Chrome did not connect") {
		t.Fatalf("log: %q", log.String())
	}
}

func TestIsToolCall(t *testing.T) {
	for line, want := range map[string]bool{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}` + "\n": true,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`:                     false,
		`{"jsonrpc":"2.0","id":2,"result":{"method":"tools/call"}}`:          false,
		`not json`: false,
	} {
		if got := isToolCall([]byte(line)); got != want {
			t.Errorf("isToolCall(%s) = %v", line, got)
		}
	}
}
