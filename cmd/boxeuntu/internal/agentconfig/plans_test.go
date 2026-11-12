package agentconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverReadsSubscriptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A Claude subscription alone gives the base URL too, without a key.
		w.Write([]byte("export ANTHROPIC_BASE_URL=http://gw/anthropic\nexport CLAUDE_CODE_OAUTH_TOKEN=boxe\nexport BOXE_LLM_PLANS=\"claude chatgpt\"\n"))
	}))
	defer srv.Close()
	g, err := Discover(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if g != (Gateway{URL: srv.URL, Claude: true, ChatGPT: true}) {
		t.Fatalf("gateway = %+v", g)
	}
}

func TestCodexUsesTheChatGPTSubscription(t *testing.T) {
	for _, g := range []Gateway{{URL: "http://gw", ChatGPT: true}, {URL: "http://gw", ChatGPT: true, OpenAI: true}} {
		var codex string
		for _, f := range Files(g, false, false) {
			if f.Agent == "codex" {
				codex = string(f.Content)
			}
		}
		if !strings.Contains(codex, `base_url = "http://gw/chatgpt"`) || !strings.Contains(codex, `experimental_bearer_token = "boxe"`) ||
			!strings.Contains(codex, `wire_api = "responses"`) || strings.Contains(codex, "/openai") {
			t.Errorf("codex with %+v:\n%s", g, codex)
		}
	}
}

// OpenCode runs Claude Code itself (its Claude Agent provider), without
// ~/.claude/settings.json: the script it runs instead drops the API key of
// the login shell, which would win over the subscription.
func TestOpenCodeRunsClaudeCodeOnTheSubscription(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	if _, err := Apply(home, Files(Gateway{URL: "http://gw", Anthropic: true, Claude: true}, false, false)); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, ClaudeSubscriptionScript)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho \"key=${ANTHROPIC_API_KEY-unset} token=$CLAUDE_CODE_OAUTH_TOKEN args=$*\"\n"), 0o755)
	cmd := exec.Command(script, "-p", "hi")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ANTHROPIC_API_KEY=boxe", "CLAUDE_CODE_OAUTH_TOKEN=boxe"}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "key=unset token=boxe args=-p hi\n" {
		t.Fatalf("%s: %q %v", script, out, err)
	}

	// Without the subscription it goes.
	if _, err := Apply(home, Files(Gateway{URL: "http://gw", Anthropic: true}, false, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("%s still there: %v", script, err)
	}
}

func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return s
}

func TestClaudeCodeUsesTheClaudeSubscription(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude/settings.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	// The user's settings stay as they are.
	os.WriteFile(path, []byte(`{"model":"opus","env":{"FOO":"bar"}}`), 0o644)

	g := Gateway{URL: "http://gw", Claude: true, Anthropic: true}
	if status, err := ClaudeSettings(home, g); err != nil || status != "written" {
		t.Fatalf("ClaudeSettings: %q %v", status, err)
	}
	s := readSettings(t, home)
	env, _ := s["env"].(map[string]any)
	if s["model"] != "opus" || env["FOO"] != "bar" || env["ANTHROPIC_BASE_URL"] != "http://gw/anthropic" ||
		env["CLAUDE_CODE_OAUTH_TOKEN"] != "boxe" || env["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("settings: %v", s)
	}
	if status, _ := ClaudeSettings(home, g); status != "unchanged" {
		t.Errorf("second run: %q", status)
	}

	// Claude Code rewrites the file, changing other settings: ours stay ours.
	s["model"] = "sonnet"
	b, _ := json.Marshal(s)
	os.WriteFile(path, b, 0o644)
	// The subscription goes: our variables go, the user's stay.
	if status, err := ClaudeSettings(home, Gateway{URL: "http://gw", Anthropic: true}); err != nil || status != "removed" {
		t.Fatalf("without the subscription: %q %v", status, err)
	}
	s = readSettings(t, home)
	env, _ = s["env"].(map[string]any)
	if s["model"] != "sonnet" || env["FOO"] != "bar" || len(env) != 1 {
		t.Errorf("settings after: %v", s)
	}
}

func TestClaudeSettingsLeavesTheUsersOwnAlone(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude/settings.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://my-proxy"}}`), 0o644)
	if _, err := ClaudeSettings(home, Gateway{URL: "http://gw", Claude: true}); err != nil {
		t.Fatal(err)
	}
	env, _ := readSettings(t, home)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://my-proxy" || env["CLAUDE_CODE_OAUTH_TOKEN"] != "boxe" {
		t.Errorf("env: %v", env)
	}
	// Removed later, the user's own value is still there.
	ClaudeSettings(home, Gateway{URL: "http://gw"})
	env, _ = readSettings(t, home)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://my-proxy" || env["CLAUDE_CODE_OAUTH_TOKEN"] != nil {
		t.Errorf("env after: %v", env)
	}

	// A file that is not JSON is not touched.
	os.WriteFile(path, []byte("{ not json"), 0o644)
	if status, err := ClaudeSettings(home, Gateway{URL: "http://gw", Claude: true}); err != nil || status != "kept" {
		t.Errorf("broken file: %q %v", status, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{ not json" {
		t.Errorf("broken file changed: %s", b)
	}

	// No subscription and no file: nothing to do.
	if status, _ := ClaudeSettings(t.TempDir(), Gateway{URL: "http://gw"}); status != "none" {
		t.Errorf("no file: %q", status)
	}
}
