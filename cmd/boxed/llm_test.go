package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeProvider stands in for api.anthropic.com and api.openai.com: it
// records what reaches it and answers with what the test sets.
type fakeProvider struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []*http.Request
	bodies   []string
	answer   string // body of the answer
	sse      bool   // answer as a stream of server-sent events
	validKey string // /models answers 401 to other keys
}

func newFakeProvider(t *testing.T) *fakeProvider {
	f := &fakeProvider{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.bodies = append(f.bodies, string(body))
		answer, sse, valid := f.answer, f.sse, f.validKey
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			key := r.Header.Get("X-Api-Key") + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if key != valid {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"invalid key"}`)
				return
			}
			io.WriteString(w, `{"data":[]}`)
			return
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		io.WriteString(w, answer)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) last() (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1], f.bodies[len(f.bodies)-1]
}

// llmCLI is boxe with the gateway on, its providers faked.
func llmCLI(t *testing.T, users ...string) (*cli, *fakeProvider, *fakeProvider) {
	c := newCLI(t, users...)
	anthropic, openai := newFakeProvider(t), newFakeProvider(t)
	anthropic.validKey, openai.validKey = "sk-ant-alice", "sk-oai-alice"
	g := newGateway(c.box.store, "10.42.0.1:2998")
	g.upstream["anthropic"], g.upstream["openai"] = anthropic.URL, openai.URL+"/v1"
	c.box.llm = g
	c.box.hv.LLMGateway = g.URL()
	return c, anthropic, openai
}

// setKey is `ssh boxe llm set <provider>` with the key on standard input.
func (c *cli) setKey(user, provider, key string) (string, string, bool) {
	c.t.Helper()
	s, err := c.conn[user].NewSession()
	if err != nil {
		c.t.Fatal(err)
	}
	defer s.Close()
	var o, e strings.Builder
	s.Stdout, s.Stderr, s.Stdin = &o, &e, strings.NewReader(key+"\n")
	err = s.Run("llm set " + provider)
	return o.String(), e.String(), err == nil
}

// fromVM is a request a program in the VM at ip makes to the gateway.
func fromVM(g *Gateway, ip, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://10.42.0.1:2998"+path, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestLLMKeys(t *testing.T) {
	c, _, _ := llmCLI(t, "alice", "bob")
	if out := c.must("alice", "llm"); !strings.Contains(out, "anthropic") || !strings.Contains(out, "openai") || !strings.Contains(out, "not set") {
		t.Errorf("llm before any key:\n%s", out)
	}
	// A key the provider refuses is not saved.
	if _, errOut, ok := c.setKey("alice", "anthropic", "sk-ant-wrong"); ok || !strings.Contains(errOut, "refused") {
		t.Errorf("wrong key: %q", errOut)
	}
	if _, errOut, ok := c.setKey("alice", "anthropic", "sk-ant-alice"); !ok {
		t.Fatalf("set: %s", errOut)
	}
	out := c.must("alice", "llm")
	if strings.Contains(out, "sk-ant-alice") || !strings.Contains(out, "…lice") {
		t.Errorf("llm shows the key, or not its end:\n%s", out)
	}
	if out := c.must("bob", "llm"); strings.Contains(out, "lice") {
		t.Errorf("bob sees alice's key:\n%s", out)
	}
	var list []struct {
		Provider string `json:"provider"`
		Set      bool   `json:"set"`
		BaseURL  string `json:"base_url"`
	}
	c.json("alice", "llm --json", &list)
	if len(list) != 2 || list[0].Provider != "anthropic" || !list[0].Set || list[1].Set ||
		list[0].BaseURL != "http://10.42.0.1:2998/anthropic" {
		t.Errorf("llm --json: %+v", list)
	}
	if _, _, ok := c.setKey("alice", "gemini", "x"); ok {
		t.Error("set of an unknown provider succeeded")
	}
	c.must("alice", "llm rm anthropic")
	if out := c.must("alice", "llm"); strings.Contains(out, "lice") {
		t.Errorf("after rm:\n%s", out)
	}
}

// Without the gateway the command says so.
func TestLLMOff(t *testing.T) {
	c := newCLI(t, "alice")
	if _, errOut, ok := c.run("alice", "llm"); ok || !strings.Contains(errOut, "not on") {
		t.Errorf("llm without the gateway: %q", errOut)
	}
}

func TestGatewayAddsTheOwnersKey(t *testing.T) {
	c, anthropic, _ := llmCLI(t, "alice", "bob")
	c.must("alice", "new test") // 10.42.0.2
	c.must("bob", "new other")  // 10.42.0.3
	c.setKey("alice", "anthropic", "sk-ant-alice")
	g := c.box.llm

	anthropic.answer = `{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0}}`
	w := fromVM(g, "10.42.0.2", "POST", "/anthropic/v1/messages", `{"model":"claude-sonnet-5-5"}`,
		map[string]string{"X-Api-Key": "implicit", "Anthropic-Version": "2023-06-01"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"m1"`) {
		t.Fatalf("answer %d: %s", w.Code, w.Body)
	}
	r, body := anthropic.last()
	if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "sk-ant-alice" || r.Header.Get("Anthropic-Version") != "2023-06-01" ||
		body != `{"model":"claude-sonnet-5-5"}` {
		t.Errorf("upstream got %s %v %q", r.URL.Path, r.Header, body)
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		t.Error("the VM's address went to the provider")
	}

	// bob has no key: the error says how to add one.
	w = fromVM(g, "10.42.0.3", "POST", "/anthropic/v1/messages", `{}`, map[string]string{"X-Api-Key": "implicit"})
	if w.Code != 401 || !strings.Contains(w.Body.String(), "ssh boxe llm set anthropic") {
		t.Errorf("no key: %d %s", w.Code, w.Body)
	}
	// Not a VM of boxe.
	if w := fromVM(g, "10.42.0.99", "POST", "/anthropic/v1/messages", `{}`, nil); w.Code != 403 {
		t.Errorf("unknown address: %d", w.Code)
	}

	// A key of one's own goes through as it is, and is counted too.
	fromVM(g, "10.42.0.3", "POST", "/anthropic/v1/messages", `{}`, map[string]string{"X-Api-Key": "sk-ant-bobs-own"})
	if r, _ := anthropic.last(); r.Header.Get("X-Api-Key") != "sk-ant-bobs-own" {
		t.Errorf("own key replaced: %q", r.Header.Get("X-Api-Key"))
	}

	var usage []llmUsageJSON
	c.json("alice", "llm usage --json", &usage)
	if len(usage) != 1 {
		t.Fatalf("usage: %+v", usage)
	}
	u := usage[0]
	// Claude Sonnet 5.5: $2 in, $10 out, $0.20 cache reads per million.
	if u.VM != "test" || u.Model != "claude-sonnet-5-5" || u.Requests != 1 || u.Input != 1000 || u.Output != 200 ||
		u.CacheRead != 5000 || u.CostUSD == nil || *u.CostUSD < 0.00499 || *u.CostUSD > 0.00501 {
		t.Errorf("usage: %+v", u)
	}
	if out := c.must("alice", "llm usage"); !strings.Contains(out, "test") || !strings.Contains(out, "claude-sonnet-5-5") || !strings.Contains(out, "$0.0050") {
		t.Errorf("llm usage:\n%s", out)
	}
	c.json("bob", "llm usage --json", &usage)
	if len(usage) != 1 || usage[0].VM != "other" || usage[0].CostUSD == nil {
		t.Errorf("bob's usage: %+v", usage)
	}
}

func TestGatewayCountsStreams(t *testing.T) {
	c, anthropic, openai := llmCLI(t, "alice")
	c.must("alice", "new test")
	c.setKey("alice", "anthropic", "sk-ant-alice")
	c.setKey("alice", "openai", "sk-oai-alice")
	g := c.box.llm

	anthropic.sse = true
	anthropic.answer = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":100,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"
	w := fromVM(g, "10.42.0.2", "POST", "/anthropic/v1/messages", `{"stream":true}`, nil)
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Errorf("stream not passed on: %s", w.Body)
	}

	// Chat completions tell the tokens of a stream only if asked: the
	// gateway asks.
	openai.sse = true
	openai.answer = "data: {\"model\":\"gpt-5-mini\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-5-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\n" +
		"data: [DONE]\n\n"
	fromVM(g, "10.42.0.2", "POST", "/openai/chat/completions", `{"model":"gpt-5-mini","stream":true,"messages":[]}`,
		map[string]string{"Authorization": "Bearer implicit", "Content-Type": "application/json"})
	r, body := openai.last()
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-oai-alice" {
		t.Errorf("openai upstream got %s %q", r.URL.Path, r.Header.Get("Authorization"))
	}
	var req struct {
		Model         string `json:"model"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal([]byte(body), &req); !req.StreamOptions.IncludeUsage || req.Model != "gpt-5-mini" {
		t.Errorf("request to openai: %s", body)
	}

	// The Responses API.
	openai.sse = false
	openai.answer = `{"model":"gpt-5","usage":{"input_tokens":2000,"output_tokens":100,"input_tokens_details":{"cached_tokens":0}}}`
	fromVM(g, "10.42.0.2", "POST", "/openai/responses", `{"model":"gpt-5"}`, nil)

	var usage []llmUsageJSON
	c.json("alice", "llm usage --json", &usage)
	byModel := map[string]llmUsageJSON{}
	for _, u := range usage {
		byModel[u.Model] = u
	}
	if u := byModel["claude-opus-5-5"]; u.Input != 10 || u.CacheWrite != 100 || u.Output != 50 {
		t.Errorf("anthropic stream: %+v", u)
	}
	if u := byModel["gpt-5-mini"]; u.Input != 200 || u.CacheRead != 100 || u.Output != 20 {
		t.Errorf("chat completions stream: %+v", u)
	}
	if u := byModel["gpt-5"]; u.Input != 2000 || u.Output != 100 || u.CostUSD == nil {
		t.Errorf("responses: %+v", u)
	}
}

// What programs in the VM need to use the gateway: shells take it from
// /env, Shelley from /shelley.json. Only for providers with a key.
func TestGatewaySetup(t *testing.T) {
	c, _, _ := llmCLI(t, "alice", "bob")
	c.must("alice", "new test")
	c.must("bob", "new other")
	g := c.box.llm
	if w := fromVM(g, "10.42.0.2", "GET", "/env", "", nil); strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("env without keys: %q", w.Body)
	}
	if w := fromVM(g, "10.42.0.2", "GET", "/shelley.json", "", nil); strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("shelley.json without keys: %q", w.Body)
	}
	c.setKey("alice", "anthropic", "sk-ant-alice")
	env := fromVM(g, "10.42.0.2", "GET", "/env", "", nil).Body.String()
	if !strings.Contains(env, "export ANTHROPIC_BASE_URL=http://10.42.0.1:2998/anthropic\n") || !strings.Contains(env, "export ANTHROPIC_API_KEY=") ||
		strings.Contains(env, "OPENAI") || strings.Contains(env, "sk-ant-alice") {
		t.Errorf("env: %q", env)
	}
	if w := fromVM(g, "10.42.0.2", "GET", "/shelley.json", "", nil); !strings.Contains(w.Body.String(), `"llm_gateway":"http://10.42.0.1:2998"`) {
		t.Errorf("shelley.json: %q", w.Body)
	}
	if w := fromVM(g, "10.42.0.3", "GET", "/env", "", nil); strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("bob's env: %q", w.Body)
	}
	// Shelley reads its configuration when it starts: the first key, and
	// the last one removed, restart it in the owner's VMs.
	if !c.host.ranIn("test", "systemctl try-restart shelley") {
		t.Error("Shelley not restarted after the first key")
	}
	if c.host.ranIn("other", "systemctl try-restart shelley") {
		t.Error("Shelley restarted in bob's VM")
	}
}

// New VMs and copies get what makes their shells and Shelley use the gateway.
func TestConfigArchiveSetsUpTheGateway(t *testing.T) {
	c, _, _ := llmCLI(t, "alice")
	files := archiveFiles(t, c.box.hv, VM{Name: "test", Index: 2})
	if !strings.Contains(files["etc/profile.d/boxe-llm.sh"], "http://10.42.0.1:2998/env") {
		t.Errorf("profile: %q", files["etc/profile.d/boxe-llm.sh"])
	}
	if s := files["etc/systemd/system/shelley.service.d/llm.conf"]; !strings.Contains(s, "http://10.42.0.1:2998/shelley.json") || !strings.Contains(s, "-config ") {
		t.Errorf("Shelley: %q", s)
	}
	if !strings.Contains(files["home/exedev/.config/boxe/AGENTS.md"], "ssh boxe llm set") {
		t.Error("AGENTS.md does not tell about the keys")
	}
	c.box.hv.LLMGateway = ""
	if _, ok := archiveFiles(t, c.box.hv, VM{Name: "test", Index: 2})["etc/profile.d/boxe-llm.sh"]; ok {
		t.Error("gateway files without the gateway")
	}
}

func TestPrices(t *testing.T) {
	for _, c := range []struct {
		model string
		known bool
	}{{"claude-opus-5-5", true}, {"claude-sonnet-4-5-20250929", true}, {"gpt-5-mini-2025-08-07", true}, {"gpt-4o-mini", true}, {"some-new-model", false},
		// Not priced like the model they start with.
		{"o3-pro", false}, {"gpt-5-pro", false}, {"gpt-5.5", false}, {"claude-opus-5-5-fast", false}} {
		if _, ok := priceOf(c.model); ok != c.known {
			t.Errorf("%s: known %v", c.model, ok)
		}
	}
	// The longest prefix wins: gpt-5-mini is not gpt-5.
	mini, _ := priceOf("gpt-5-mini-2025-08-07")
	full, _ := priceOf("gpt-5")
	if mini.Input >= full.Input {
		t.Errorf("gpt-5-mini priced like gpt-5: %+v %+v", mini, full)
	}
}

// archiveFiles are the files of v's config archive, by path.
func archiveFiles(t *testing.T, h *Hypervisor, v VM) map[string]string {
	t.Helper()
	data, err := h.configArchive(v, "")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[hdr.Name] = string(b)
	}
}

// ranIn says whether a command starting with prefix ran in the VM vm.
func (f *fakeHost) ranIn(vm, prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "ssh ") && strings.Contains(c, "HostKeyAlias="+vm+".") && strings.Contains(c, " "+prefix) {
			return true
		}
	}
	return false
}

// The database holds the API keys: only root may read it.
func TestDatabaseIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boxe.db")
	os.WriteFile(path, nil, 0o644) // made by an older boxed
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetLLMKey("alice", "anthropic", "sk")
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", p, fi.Mode(), err)
		}
	}
}

// Paths outside the provider's API are refused, whatever their dots.
func TestGatewayStaysInTheAPI(t *testing.T) {
	c, _, openai := llmCLI(t, "alice")
	c.must("alice", "new test")
	c.setKey("alice", "openai", "sk-oai-alice")
	for _, p := range []string{"/openai/../dashboard", "/openai/%2e%2e/x", "/openaix/models", "/openai/v1/../../x"} {
		r := httptest.NewRequest("GET", "http://10.42.0.1:2998/", nil)
		r.URL.Path, r.URL.RawPath = p, ""
		if strings.Contains(p, "%") {
			r.URL.RawPath = p
			r.URL.Path = strings.ReplaceAll(p, "%2e", ".")
		}
		r.RemoteAddr = "10.42.0.2:40000"
		w := httptest.NewRecorder()
		n := len(openai.reqs)
		c.box.llm.ServeHTTP(w, r)
		if w.Code != 404 || len(openai.reqs) != n {
			t.Errorf("%s: %d, reached the provider: %v", p, w.Code, len(openai.reqs) != n)
		}
	}
}

// A body too large to look at goes on as it is.
func TestGatewayLargeRequest(t *testing.T) {
	c, _, openai := llmCLI(t, "alice")
	c.must("alice", "new test")
	c.setKey("alice", "openai", "sk-oai-alice")
	big := `{"stream":true,"messages":"` + strings.Repeat("x", requestLimit) + `"}`
	r := httptest.NewRequest("POST", "http://10.42.0.1:2998/openai/chat/completions", io.MultiReader(strings.NewReader(big)))
	r.ContentLength = -1 // chunked
	r.RemoteAddr = "10.42.0.2:40000"
	c.box.llm.ServeHTTP(httptest.NewRecorder(), r)
	if _, body := openai.last(); body != big {
		t.Errorf("large body changed: %d bytes, want %d", len(body), len(big))
	}
}

// An event too long to keep does not stop the counting of later ones, and
// the last line counts without a newline.
func TestMeterOverflow(t *testing.T) {
	var got llmUsage
	m := &meter{ReadCloser: io.NopCloser(strings.NewReader("data: {\"x\":\"" + strings.Repeat("y", meterLimit+10) + "\"}\n" +
		"data: {\"model\":\"m\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}")), sse: true, record: func(u llmUsage) { got = u }}
	io.Copy(io.Discard, m)
	m.Close()
	if got.Model != "m" || got.Input != 7 || got.Output != 3 {
		t.Errorf("usage after a long event: %+v", got)
	}
}
