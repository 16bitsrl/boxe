package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	gossh "golang.org/x/crypto/ssh"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChatGPTAuth stands in for auth.openai.com: it hands out tokens for a
// refresh token once, as OpenAI does, and refuses one used before.
type fakeChatGPTAuth struct {
	*httptest.Server
	mu       sync.Mutex
	next     int           // the number of the next tokens
	valid    string        // the refresh token it takes
	lifetime time.Duration // of the access tokens it hands out
	calls    int
	bodies   []map[string]string
	arrived  chan struct{} // if set, told of the next refresh, which waits for hold
	hold     chan struct{}
}

func newFakeChatGPTAuth(t *testing.T) *fakeChatGPTAuth {
	f := &fakeChatGPTAuth{valid: "rt-1", next: 2, lifetime: 10 * 24 * time.Hour}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		arrived, hold := f.arrived, f.hold
		f.arrived, f.hold = nil, nil
		f.mu.Unlock()
		if arrived != nil {
			close(arrived)
			<-hold
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		f.bodies = append(f.bodies, body)
		time.Sleep(20 * time.Millisecond) // long enough for requests to pile up
		if r.URL.Path != "/oauth/token" || body["grant_type"] != "refresh_token" || body["refresh_token"] != f.valid {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"refresh_token_reused","message":"already used"}}`)
			return
		}
		n := f.next
		f.next++
		f.valid = fmt.Sprintf("rt-%d", n)
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  testJWT(time.Now().Add(f.lifetime), fmt.Sprintf("at-%d", n)),
			"refresh_token": f.valid,
			"id_token":      testJWT(time.Now().Add(time.Hour), "id"),
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeChatGPTAuth) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testJWT is a token that expires at exp; jti tells tokens apart.
func testJWT(exp time.Time, jti string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(map[string]any{"exp": exp.Unix(), "jti": jti}) + ".sig"
}

// jtiOf is the jti of a bearer token made by testJWT.
func jtiOf(t *testing.T, bearer string) string {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(bearer, "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatalf("not a token: %q", bearer)
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct{ Jti string }
	json.Unmarshal(b, &claims)
	return claims.Jti
}

// authJSON is ~/.codex/auth.json after `codex login`, as Codex writes it.
func authJSON(refresh string) string {
	return `{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": "` + testJWT(time.Now().Add(time.Hour), "id") + `",
    "access_token": "` + testJWT(time.Now().Add(time.Hour), "at-1") + `",
    "refresh_token": "` + refresh + `",
    "account_id": "acc-alice"
  },
  "last_refresh": "2026-11-10T10:00:00Z"
}`
}

// planCLI is boxe with the gateway on and the subscriptions' services faked.
func planCLI(t *testing.T, users ...string) (*cli, *fakeProvider, *fakeProvider, *fakeChatGPTAuth) {
	c, anthropic, _ := llmCLI(t, users...)
	chatgpt, auth := newFakeProvider(t), newFakeChatGPTAuth(t)
	anthropic.validKey = "sk-ant-oat01-alice" // the API's /models takes the subscription's token too
	g := c.box.llm
	g.upstream["claude"] = anthropic.URL
	g.upstream["chatgpt"] = chatgpt.URL + "/backend-api/codex"
	g.chatgptTokenURL = auth.URL + "/oauth/token"
	return c, anthropic, chatgpt, auth
}

// claudeCode are the headers of Claude Code signed in with the boxe placeholder.
var claudeCode = map[string]string{
	"Authorization":     "Bearer boxe",
	"Anthropic-Beta":    "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14",
	"Anthropic-Version": "2023-06-01",
}

func TestClaudeSubscription(t *testing.T) {
	c, anthropic, _, _ := planCLI(t, "alice", "bob")
	c.must("alice", "new test") // 10.42.0.2
	c.must("bob", "new other")  // 10.42.0.3
	g := c.box.llm

	if _, errOut, ok := c.setKey("alice", "claude", "sk-ant-oat01-wrong"); ok || !strings.Contains(errOut, "refused") {
		t.Errorf("wrong token: %q", errOut)
	}
	if out, errOut, ok := c.setKey("alice", "claude", "sk-ant-oat01-alice"); !ok || !strings.Contains(out, "Claude Code") {
		t.Fatalf("set claude: %q %q", out, errOut)
	}
	r, _ := anthropic.last()
	if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-alice" || !strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		t.Errorf("token checked with %v", r.Header)
	}
	out := c.must("alice", "llm")
	if !strings.Contains(out, "claude") || !strings.Contains(out, "…lice") || strings.Contains(out, "oat01-alice") {
		t.Errorf("llm:\n%s", out)
	}
	if !c.host.ranIn("test", "boxeuntu configure") {
		t.Error("agents not set up after the subscription")
	}
	if c.host.ranIn("test", "sudo systemctl try-restart shelley") {
		t.Error("Shelley restarted: it does not use subscriptions")
	}

	// Claude Code's placeholder gets the owner's subscription.
	anthropic.answer = `{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":100,"output_tokens":20}}`
	w := llmRequest(g, "10.42.0.2", "POST", "/anthropic/v1/messages", `{"model":"claude-opus-5-5"}`, claudeCode)
	if w.Code != 200 {
		t.Fatalf("answer %d: %s", w.Code, w.Body)
	}
	r, _ = anthropic.last()
	if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer sk-ant-oat01-alice" || r.Header.Get("X-Api-Key") != "" ||
		r.Header.Get("Anthropic-Beta") != claudeCode["Anthropic-Beta"] {
		t.Errorf("upstream got %s %v", r.URL.Path, r.Header)
	}

	// With an API key too, programs that send the key's placeholder get the key.
	c.setKey("alice", "anthropic", "sk-ant-oat01-alice") // the fake takes one key: the same will do
	llmRequest(g, "10.42.0.2", "POST", "/anthropic/v1/messages", `{}`, map[string]string{"X-Api-Key": "boxe"})
	if r, _ := anthropic.last(); r.Header.Get("X-Api-Key") != "sk-ant-oat01-alice" || r.Header.Get("Authorization") != "" {
		t.Errorf("API key request: %v", r.Header)
	}

	// bob has no subscription: the error says how to add one.
	w = llmRequest(g, "10.42.0.3", "POST", "/anthropic/v1/messages", `{}`, claudeCode)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "ssh boxe llm set claude") {
		t.Errorf("no subscription: %d %s", w.Code, w.Body)
	}

	// Usage: counted apart from the key's, without a price.
	var usage []llmUsageJSON
	c.json("alice", "llm usage --json", &usage)
	if len(usage) != 2 || usage[0].Provider != "anthropic" || usage[0].CostUSD == nil ||
		usage[1].Provider != "claude" || usage[1].Input != 100 || usage[1].CostUSD != nil {
		t.Errorf("usage: %+v", usage)
	}
	if out := c.must("alice", "llm usage"); !strings.Contains(out, "subscription") || strings.Contains(out, "price boxe does not know") {
		t.Errorf("llm usage:\n%s", out)
	}

	c.must("alice", "llm rm claude")
	if w := llmRequest(g, "10.42.0.2", "POST", "/anthropic/v1/messages", `{}`, claudeCode); w.Code != 401 {
		t.Errorf("after rm: %d", w.Code)
	}
}

func TestSubscriptionsInTheVM(t *testing.T) {
	c, _, _, _ := planCLI(t, "alice")
	c.must("alice", "new test")
	g := c.box.llm
	c.setKey("alice", "claude", "sk-ant-oat01-alice")
	env := llmRequest(g, "10.42.0.2", "GET", "/env", "", nil).Body.String()
	if !strings.Contains(env, "export ANTHROPIC_BASE_URL=http://10.42.0.1:2998/anthropic\n") ||
		!strings.Contains(env, "export CLAUDE_CODE_OAUTH_TOKEN=boxe\n") || !strings.Contains(env, "export BOXE_LLM_PLANS=claude\n") ||
		strings.Contains(env, "ANTHROPIC_API_KEY") || strings.Contains(env, "oat01") || strings.Contains(env, "OPENCODE") {
		t.Errorf("env with the subscription alone: %q", env)
	}
	// Shelley cannot use a subscription.
	if w := llmRequest(g, "10.42.0.2", "GET", "/shelley.json", "", nil); strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("shelley.json: %q", w.Body)
	}
	c.setKey("alice", "chatgpt", authJSON("rt-1"))
	c.setKey("alice", "anthropic", "sk-ant-oat01-alice")
	env = llmRequest(g, "10.42.0.2", "GET", "/env", "", nil).Body.String()
	if strings.Count(env, "ANTHROPIC_BASE_URL") != 1 || !strings.Contains(env, "export ANTHROPIC_API_KEY=boxe\n") ||
		!strings.Contains(env, "export BOXE_LLM_PLANS=\"claude chatgpt\"\n") || strings.Contains(env, "OPENAI") {
		t.Errorf("env with both: %q", env)
	}
	// OpenCode's Claude Code gets the subscription through boxeuntu's
	// script, once boxeuntu has written it.
	script := `"$HOME/.config/boxeuntu/claude-subscription"`
	if !strings.Contains(env, "[ -x "+script+" ] && export OPENCODE_CLAUDE_PATH="+script+"\n") {
		t.Errorf("env with both, for OpenCode: %q", env)
	}
}

func TestChatGPTSubscription(t *testing.T) {
	c, _, chatgpt, auth := planCLI(t, "alice", "bob")
	c.must("alice", "new test")
	c.must("bob", "new other")
	g := c.box.llm

	if _, errOut, ok := c.setKey("alice", "chatgpt", `{"tokens":{}}`); ok || !strings.Contains(errOut, "auth.json") {
		t.Errorf("auth.json without tokens: %q", errOut)
	}
	if _, errOut, ok := c.setKey("alice", "chatgpt", authJSON("rt-old")); ok || !strings.Contains(errOut, "refused") {
		t.Errorf("used refresh token: %q", errOut)
	}
	// boxed takes the login over at once: its refresh token is the only one left.
	if out, errOut, ok := c.setKey("alice", "chatgpt", authJSON("rt-1")); !ok || !strings.Contains(out, "Codex") {
		t.Fatalf("set chatgpt: %q %q", out, errOut)
	}
	if auth.refreshes() == 0 || auth.bodies[len(auth.bodies)-1]["client_id"] == "" {
		t.Errorf("login not refreshed when saved: %v", auth.bodies)
	}
	if out := c.must("alice", "llm"); !strings.Contains(out, "chatgpt") || strings.Contains(out, "rt-") || strings.Contains(out, "at-") {
		t.Errorf("llm:\n%s", out)
	}

	chatgpt.sse = true
	chatgpt.untyped = true
	chatgpt.answer = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":300,\"output_tokens\":40,\"input_tokens_details\":{\"cached_tokens\":100}}}}\n\n"
	w := llmRequest(g, "10.42.0.2", "POST", "/chatgpt/responses", `{"model":"gpt-6.1-sol","stream":true}`,
		map[string]string{"Authorization": "Bearer boxe", "Chatgpt-Account-Id": "someone-else"})
	if w.Code != 200 {
		t.Fatalf("answer %d: %s", w.Code, w.Body)
	}
	r, _ := chatgpt.last()
	if r.URL.Path != "/backend-api/codex/responses" || jtiOf(t, r.Header.Get("Authorization")) != "at-2" || r.Header.Get("Chatgpt-Account-Id") != "acc-alice" {
		t.Errorf("upstream got %s %v", r.URL.Path, r.Header)
	}
	var usage []llmUsageJSON
	c.json("alice", "llm usage --json", &usage)
	if len(usage) != 1 || usage[0].Provider != "chatgpt" || usage[0].Input != 200 || usage[0].CacheRead != 100 || usage[0].CostUSD != nil {
		t.Errorf("usage: %+v", usage)
	}

	// bob has no subscription.
	if w := llmRequest(g, "10.42.0.3", "POST", "/chatgpt/responses", `{}`, map[string]string{"Authorization": "Bearer boxe"}); w.Code != 401 ||
		!strings.Contains(w.Body.String(), "ssh boxe llm set chatgpt") {
		t.Errorf("no subscription: %d %s", w.Code, w.Body)
	}
	// Only the gateway signs in: a token of the VM's own is not passed on.
	llmRequest(g, "10.42.0.2", "POST", "/chatgpt/responses", `{}`, map[string]string{"Authorization": "Bearer something-else"})
	if r, _ := chatgpt.last(); jtiOf(t, r.Header.Get("Authorization")) != "at-2" {
		t.Errorf("own token: %v", r.Header)
	}
}

// An access token about to expire is refreshed once, however many
// requests wait for it, and the new refresh token is kept.
func TestChatGPTRefresh(t *testing.T) {
	c, _, chatgpt, auth := planCLI(t, "alice")
	c.must("alice", "new test")
	g := c.box.llm
	auth.lifetime = time.Minute // within the margin: the next request refreshes
	c.setKey("alice", "chatgpt", authJSON("rt-1"))
	auth.mu.Lock()
	auth.lifetime = 10 * 24 * time.Hour
	auth.mu.Unlock()
	before := auth.refreshes()

	chatgpt.answer = `{}`
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := llmRequest(g, "10.42.0.2", "POST", "/chatgpt/responses", `{}`, map[string]string{"Authorization": "Bearer boxe"}); w.Code != 200 {
				t.Errorf("answer %d: %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if n := auth.refreshes() - before; n != 1 {
		t.Errorf("%d refreshes", n)
	}
	if r, _ := chatgpt.last(); jtiOf(t, r.Header.Get("Authorization")) != "at-3" {
		t.Errorf("not the new token: %v", r.Header)
	}

	// A login OpenAI no longer takes: the VM is told to sign in again.
	auth.mu.Lock()
	auth.valid = "nothing"
	auth.mu.Unlock()
	keys, _ := c.box.store.LLMKeys("alice")
	var login chatgptLogin
	json.Unmarshal([]byte(keys["chatgpt"]), &login)
	login.Expires = time.Now().Add(-time.Minute).Unix()
	b, _ := json.Marshal(login)
	c.box.store.SetLLMKey("alice", "chatgpt", string(b))
	w := llmRequest(g, "10.42.0.2", "POST", "/chatgpt/responses", `{}`, map[string]string{"Authorization": "Bearer boxe"})
	if w.Code != 401 || !strings.Contains(w.Body.String(), "ssh boxe llm set chatgpt") {
		t.Errorf("refresh refused: %d %s", w.Code, w.Body)
	}
}

// A refresh under way when the member removes the login does not bring it
// back.
func TestChatGPTRemovedWhileRefreshing(t *testing.T) {
	c, _, chatgpt, auth := planCLI(t, "alice")
	c.must("alice", "new test")
	g := c.box.llm
	auth.lifetime = time.Minute // the next request refreshes
	c.setKey("alice", "chatgpt", authJSON("rt-1"))
	chatgpt.answer = `{}`
	arrived, hold := make(chan struct{}), make(chan struct{})
	auth.mu.Lock()
	auth.arrived, auth.hold = arrived, hold
	auth.mu.Unlock()

	request := make(chan struct{})
	go func() {
		defer close(request)
		llmRequest(g, "10.42.0.2", "POST", "/chatgpt/responses", `{}`, map[string]string{"Authorization": "Bearer boxe"})
	}()
	<-arrived
	removed := make(chan struct{})
	go func() {
		defer close(removed)
		c.must("alice", "llm rm chatgpt")
	}()
	select {
	case <-removed:
	case <-time.After(100 * time.Millisecond): // waits for the refresh
	}
	close(hold)
	<-request
	<-removed
	if keys, _ := c.box.store.LLMKeys("alice"); keys["chatgpt"] != "" {
		t.Errorf("the login came back after llm rm: %s", keys["chatgpt"])
	}
}

// With ssh -t, the instructions before the token are shown as written, not
// wrapped where the terminal counts its line as full, and the token is read.
func TestSubscriptionAskedOnATerminal(t *testing.T) {
	c, _, _, _ := planCLI(t, "alice")
	s, err := c.conn["alice"].NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RequestPty("xterm", 40, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	p := &prompt{t: t, s: s}
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
	if err := s.Start("llm set claude"); err != nil {
		t.Fatal(err)
	}
	got := p.wait("is not shown): ")
	if !strings.Contains(got, "Claude account.\r\nPaste the token") {
		t.Errorf("the instructions on the terminal:\n%q", got)
	}
	p.typ("sk-ant-oat01-alice\r")
	if got := p.wait("Claude Code"); strings.Contains(got, "oat01-alice") {
		t.Errorf("the token was shown: %q", got)
	}
}
