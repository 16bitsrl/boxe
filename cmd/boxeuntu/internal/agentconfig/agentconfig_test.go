package agentconfig

import (
	"context"
	"encoding/json"
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
	for _, f := range Files(Gateway{URL: "http://10.42.0.1:2998", Anthropic: true, OpenAI: true}) {
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
	for _, f := range Files(Gateway{URL: "http://gw", Anthropic: true}) {
		switch {
		case f.Agent == "codex" && f.Content != nil:
			t.Errorf("codex config without an OpenAI key:\n%s", f.Content)
		case f.Agent == "opencode" && (strings.Contains(string(f.Content), "openai") || !strings.Contains(string(f.Content), openCodeAnthropicModel)):
			t.Errorf("opencode with only Anthropic:\n%s", f.Content)
		}
	}
	// No key at all: no files.
	for _, f := range Files(Gateway{URL: "http://gw"}) {
		if f.Content != nil {
			t.Errorf("%s config without keys:\n%s", f.Agent, f.Content)
		}
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
	both := Files(Gateway{URL: "http://gw", Anthropic: true, OpenAI: true})

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
	onlyAnthropic := Files(Gateway{URL: "http://gw", Anthropic: true})
	if s := statuses(t, home, onlyAnthropic); s["pi"] != "kept" || s["opencode"] != "written" || s["codex"] != "kept" {
		t.Fatalf("after the user's edit: %v", s)
	}
	if read(t, pi) != `{"providers": {}}` {
		t.Fatal("the user's pi file was replaced")
	}

	// The owner removes their keys: our files go, theirs stay.
	if s := statuses(t, home, Files(Gateway{URL: "http://gw"})); s["opencode"] != "removed" || s["pi"] != "kept" || s["codex"] != "kept" {
		t.Fatalf("without keys: %v", s)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/opencode/opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("opencode config still there: %v", err)
	}
}

func TestApplyTakesOverAFileThatAlreadyMatches(t *testing.T) {
	home := t.TempDir()
	files := Files(Gateway{URL: "http://gw", OpenAI: true})
	path := filepath.Join(home, files[0].Path)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, files[0].Content, 0o600)
	if s := statuses(t, home, files); s["codex"] != "unchanged" {
		t.Fatalf("status %v", s)
	}
	// Ours now: a change of gateway rewrites it.
	if s := statuses(t, home, Files(Gateway{URL: "http://other", OpenAI: true})); s["codex"] != "written" {
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
