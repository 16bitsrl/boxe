package agentconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverReadsProvidersWithAKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/env" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("export OPENAI_BASE_URL=http://gw/openai\nexport OPENAI_API_KEY=boxe\n"))
	}))
	defer srv.Close()
	g, err := Discover(context.Background(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if g != (Gateway{URL: srv.URL, OpenAI: true}) {
		t.Fatalf("gateway = %+v", g)
	}
}

func TestDiscoverFailsWhenTheGatewayDoes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "only boxe VMs can use this gateway", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("no error for a 403")
	}
}

func TestFilesPointAgentsAtTheGateway(t *testing.T) {
	both := map[string]string{}
	for _, f := range Files(Gateway{URL: "http://10.42.0.1:2998", Anthropic: true, OpenAI: true}, false) {
		both[f.Agent] = string(f.Content)
	}
	if !strings.Contains(both["codex"], `base_url = "http://10.42.0.1:2998/openai"`) {
		t.Errorf("codex:\n%s", both["codex"])
	}
	var oc struct {
		Model    string
		Provider map[string]struct{ Options map[string]string }
	}
	if err := json.Unmarshal([]byte(both["opencode"]), &oc); err != nil {
		t.Fatal(err)
	}
	if oc.Model != openCodeAnthropicModel || oc.Provider["anthropic"].Options["baseURL"] != "http://10.42.0.1:2998/anthropic/v1" ||
		oc.Provider["openai"].Options["baseURL"] != "http://10.42.0.1:2998/openai" {
		t.Errorf("opencode:\n%s", both["opencode"])
	}
	if !strings.Contains(both["pi"], `"baseUrl": "http://10.42.0.1:2998/anthropic"`) || !strings.Contains(both["pi"], `"baseUrl": "http://10.42.0.1:2998/openai"`) {
		t.Errorf("pi:\n%s", both["pi"])
	}

	// Only Anthropic: Codex gets nothing, OpenCode starts on Claude.
	for _, f := range Files(Gateway{URL: "http://gw", Anthropic: true}, false) {
		switch {
		case f.Agent == "codex" && f.Content != nil:
			t.Errorf("codex config without an OpenAI key:\n%s", f.Content)
		case f.Agent == "opencode" && (strings.Contains(string(f.Content), "openai") || !strings.Contains(string(f.Content), openCodeAnthropicModel)):
			t.Errorf("opencode with only Anthropic:\n%s", f.Content)
		}
	}
	// No key at all: no files.
	for _, f := range Files(Gateway{URL: "http://gw"}, false) {
		if f.Content != nil {
			t.Errorf("%s config without keys:\n%s", f.Agent, f.Content)
		}
	}
}

func byPath(files []File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

// The computer tools go to every agent, with keys or without: the gateway's
// parts only with keys.
func TestFilesGiveTheComputerTools(t *testing.T) {
	const codexMCP = "[mcp_servers.computer]\ncommand = \"/usr/local/bin/boxe-computer\"\nargs = [\"mcp\"]\n"
	for _, g := range []Gateway{{}, {URL: "http://gw", Anthropic: true, OpenAI: true}} {
		f := byPath(Files(g, true))
		codex := f[".codex/config.toml"]
		if !strings.Contains(codex, codexMCP) || strings.Contains(codex, "model_provider") != g.OpenAI {
			t.Errorf("codex with %+v:\n%s", g, codex)
		}
		// TOML: the top-level key must come before any table.
		if g.OpenAI && strings.Index(codex, "model_provider") > strings.Index(codex, "[") {
			t.Errorf("codex model_provider after a table:\n%s", codex)
		}
		var oc struct {
			Model    string
			Provider map[string]any
			MCP      struct {
				Servers map[string]struct {
					Type    string
					Command []string
				}
			}
		}
		if err := json.Unmarshal([]byte(f[".config/opencode/opencode.json"]), &oc); err != nil {
			t.Fatal(err)
		}
		if c := oc.MCP.Servers["computer"]; c.Type != "local" || strings.Join(c.Command, " ") != "/usr/local/bin/boxe-computer mcp" ||
			(len(oc.Provider) > 0) != g.Anthropic || (oc.Model != "") != g.Anthropic {
			t.Errorf("opencode with %+v:\n%s", g, f[".config/opencode/opencode.json"])
		}
		if want := `{
  "mcpServers": {
    "computer": {
      "args": [
        "mcp"
      ],
      "command": "/usr/local/bin/boxe-computer"
    }
  }
}
`; f[".pi/agent/mcp.json"] != want {
			t.Errorf("pi mcp.json:\n%s", f[".pi/agent/mcp.json"])
		}
		if (f[".pi/agent/models.json"] != "") != g.Anthropic {
			t.Errorf("pi models.json with %+v:\n%s", g, f[".pi/agent/models.json"])
		}
	}
	// No boxe-computer (an older image): no pi mcp.json at all.
	if _, ok := byPath(Files(Gateway{URL: "http://gw", OpenAI: true}, false))[".pi/agent/mcp.json"]; ok {
		t.Error("pi mcp.json without the computer tools")
	}
}

// A VM on an older boxeuntu has our gateway-only files: they become ours
// with the computer tools too. A file the user changed stays theirs.
func TestApplyAddsTheComputerToolsToOurFiles(t *testing.T) {
	home := t.TempDir()
	g := Gateway{URL: "http://gw", Anthropic: true, OpenAI: true}
	statuses(t, home, Files(g, false))
	pi := filepath.Join(home, ".pi/agent/models.json")
	os.WriteFile(pi, []byte(`{"providers": {}}`), 0o600)
	s := statuses(t, home, Files(g, true))
	if s["codex"] != "written" || s["opencode"] != "written" || s["pi"] != "kept" || s["pi mcp"] != "written" {
		t.Fatalf("statuses %v", s)
	}
	if !strings.Contains(read(t, filepath.Join(home, ".codex/config.toml")), "[mcp_servers.computer]") {
		t.Fatal("codex config without the computer tools")
	}
}

func TestAddClaudeComputerOnce(t *testing.T) {
	home := t.TempDir()
	var calls []string
	there := false
	claude := func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[1] == "get" && !there {
			return errors.New("exit status 1")
		}
		there = true
		return nil
	}
	status, err := AddClaudeComputer(home, claude)
	if err != nil || status != "added" {
		t.Fatalf("first run: %q, %v", status, err)
	}
	want := []string{"mcp get computer", "mcp add --scope user computer -- /usr/local/bin/boxe-computer mcp"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %q", calls)
	}
	// The user removes them: they are not added again.
	there, calls = false, nil
	if status, err := AddClaudeComputer(home, claude); err != nil || status != "" || calls != nil {
		t.Fatalf("second run: %q, %v, calls %q", status, err, calls)
	}
}

func TestAddClaudeComputerWhenTheUserHasThem(t *testing.T) {
	home := t.TempDir()
	var calls []string
	claude := func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	if status, err := AddClaudeComputer(home, claude); err != nil || status != "there" || len(calls) != 1 {
		t.Fatalf("%q, %v, calls %q", status, err, calls)
	}
	if status, _ := AddClaudeComputer(home, claude); status != "" || len(calls) != 1 {
		t.Fatalf("second run: %q, calls %q", status, calls)
	}
}

func TestAddClaudeComputerWithoutClaude(t *testing.T) {
	home := t.TempDir()
	if status, err := AddClaudeComputer(home, nil); err != nil || status != "" {
		t.Fatalf("%q, %v", status, err)
	}
	// Claude Code installed later still gets them.
	added := false
	claude := func(args ...string) error {
		if args[1] == "get" {
			return errors.New("exit status 1")
		}
		added = true
		return nil
	}
	if status, err := AddClaudeComputer(home, claude); err != nil || status != "added" || !added {
		t.Fatalf("%q, %v", status, err)
	}
}

// A failed add is tried again at the next run.
func TestAddClaudeComputerRetriesAFailure(t *testing.T) {
	home := t.TempDir()
	fail := func(args ...string) error { return errors.New("exit status 1") }
	if _, err := AddClaudeComputer(home, fail); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(home, claudeMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker after a failure: %v", err)
	}
}

func statuses(t *testing.T, home string, files []File) map[string]string {
	t.Helper()
	res, err := Apply(home, files)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range res {
		if filepath.Base(r.Path) == "mcp.json" {
			out[r.Agent+" mcp"] = r.Status
			continue
		}
		out[r.Agent] = r.Status
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyLeavesTheUsersFilesAlone(t *testing.T) {
	home := t.TempDir()
	codex := filepath.Join(home, ".codex/config.toml")
	pi := filepath.Join(home, ".pi/agent/models.json")
	both := Files(Gateway{URL: "http://gw", Anthropic: true, OpenAI: true}, false)

	// The user's own Codex config, there before boxeuntu ran.
	os.MkdirAll(filepath.Dir(codex), 0o755)
	os.WriteFile(codex, []byte("model = \"mine\"\n"), 0o644)
	if s := statuses(t, home, both); s["codex"] != "kept" || s["opencode"] != "written" || s["pi"] != "written" {
		t.Fatalf("first run: %v", s)
	}
	if read(t, codex) != "model = \"mine\"\n" {
		t.Fatal("the user's codex config was replaced")
	}
	if fi, _ := os.Stat(pi); fi.Mode().Perm() != 0o600 {
		t.Fatalf("pi models.json mode %v", fi.Mode().Perm())
	}
	if s := statuses(t, home, both); s["opencode"] != "unchanged" || s["pi"] != "unchanged" {
		t.Fatalf("second run: %v", s)
	}

	// The user edits pi's file: from then on it is theirs.
	os.WriteFile(pi, []byte(`{"providers": {}}`), 0o600)
	onlyAnthropic := Files(Gateway{URL: "http://gw", Anthropic: true}, false)
	if s := statuses(t, home, onlyAnthropic); s["pi"] != "kept" || s["opencode"] != "written" || s["codex"] != "kept" {
		t.Fatalf("after the user's edit: %v", s)
	}
	if read(t, pi) != `{"providers": {}}` {
		t.Fatal("the user's pi file was replaced")
	}

	// The owner removes their keys: our files go, theirs stay.
	if s := statuses(t, home, Files(Gateway{URL: "http://gw"}, false)); s["opencode"] != "removed" || s["pi"] != "kept" || s["codex"] != "kept" {
		t.Fatalf("without keys: %v", s)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/opencode/opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("opencode config still there: %v", err)
	}
}

func TestApplyTakesOverAFileThatAlreadyMatches(t *testing.T) {
	home := t.TempDir()
	files := Files(Gateway{URL: "http://gw", OpenAI: true}, false)
	path := filepath.Join(home, files[0].Path)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, files[0].Content, 0o600)
	if s := statuses(t, home, files); s["codex"] != "unchanged" {
		t.Fatalf("status %v", s)
	}
	// Ours now: a change of gateway rewrites it.
	if s := statuses(t, home, Files(Gateway{URL: "http://other", OpenAI: true}, false)); s["codex"] != "written" {
		t.Fatalf("status %v", s)
	}
	if !strings.Contains(read(t, path), "http://other/openai") {
		t.Fatal("not rewritten")
	}
}

// Runs at boot and from the host can overlap: one waits for the other, or
// both would write the state file and lose track of what is ours.
func TestLockMakesRunsWait(t *testing.T) {
	home := t.TempDir()
	unlock, err := Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := Lock(home)
		if err != nil {
			t.Error(err)
		}
		u()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second run did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the second run never went on")
	}
}
