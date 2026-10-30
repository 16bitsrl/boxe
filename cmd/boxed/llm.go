package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// The LLM gateway: programs in a VM call the providers' APIs through the
// host, which adds the API key its owner saved with `ssh boxe llm set` and
// counts the tokens of every answer. Keys never enter the VMs. The VM is
// known by its source address, which the bridge filter keeps from being
// spoofed (scripts/net-setup.sh).
//
// The paths are those of exe.dev's gateway, which Shelley knows:
// /anthropic/v1/messages, /openai/chat/completions, /openai/responses.

type llmProvider struct {
	name     string
	upstream string // base URL /<name>/... maps to
	keyEnv   string // the environment variables SDKs read
	baseEnv  string
	models   string // path that checks a key
}

var llmProviders = []llmProvider{
	{"anthropic", "https://api.anthropic.com", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "/v1/models"},
	{"openai", "https://api.openai.com/v1", "OPENAI_API_KEY", "OPENAI_BASE_URL", "/models"},
}

func llmProviderNamed(name string) (llmProvider, bool) {
	for _, p := range llmProviders {
		if p.name == name {
			return p, true
		}
	}
	return llmProvider{}, false
}

// llmPlaceholder is the key programs in the VM send; the gateway puts the
// owner's in its place. Shelley sends "implicit" to exe.dev's gateway.
const llmPlaceholder = "boxe"

type Gateway struct {
	store    *Store
	addr     string            // where VMs reach it, e.g. 10.42.0.1:2998
	upstream map[string]string // provider -> base URL (tests point them elsewhere)
	client   *http.Client
}

func newGateway(store *Store, addr string) *Gateway {
	g := &Gateway{store: store, addr: addr, upstream: map[string]string{}, client: &http.Client{Timeout: 15 * time.Second}}
	for _, p := range llmProviders {
		g.upstream[p.name] = p.upstream
	}
	return g
}

// URL is the gateway as VMs reach it.
func (g *Gateway) URL() string { return "http://" + g.addr }

// vmOf is the VM a request comes from.
func (g *Gateway) vmOf(r *http.Request) (VM, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return VM{}, false
	}
	ip := net.ParseIP(host).To4()
	if ip == nil || ip[0] != 10 || ip[1] != 42 {
		return VM{}, false
	}
	v, err := g.store.ByIndex(int(ip[2])<<8 | int(ip[3]))
	return v, err == nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v, ok := g.vmOf(r)
	if !ok {
		http.Error(w, "only boxe VMs can use this gateway", http.StatusForbidden)
		return
	}
	keys, err := g.store.LLMKeys(v.Owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/env":
		// Sourced by the VM's login shells (etc/profile.d/boxe-llm.sh).
		for _, p := range llmProviders {
			if keys[p.name] != "" {
				fmt.Fprintf(w, "export %s=%s/%s\nexport %s=%s\n", p.baseEnv, g.URL(), p.name, p.keyEnv, llmPlaceholder)
			}
		}
		return
	case "/shelley.json":
		// Read by Shelley when it starts (its unit's drop-in, see configArchive).
		if len(keys) == 0 {
			io.WriteString(w, "{}\n")
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"llm_gateway": g.URL()})
		return
	}
	// Only the provider's API: no dots out of it.
	clean := path.Clean(r.URL.Path)
	name, _, _ := strings.Cut(strings.TrimPrefix(clean, "/"), "/")
	p, ok := llmProviderNamed(name)
	if !ok || clean != r.URL.Path || strings.Contains(r.URL.RawPath, "%2") || strings.Contains(r.URL.RawPath, "%5") {
		http.NotFound(w, r)
		return
	}
	// A key of the VM's own (or a subscription's token) goes through as it
	// is; the placeholder, or none, gets the owner's.
	c := credential(r)
	add := c == "" || c == llmPlaceholder || c == "implicit"
	if add && keys[p.name] == "" {
		ownKeyMissing(w, p)
		return
	}
	if !add {
		keys[p.name] = ""
	}
	g.proxy(p, v, keys[p.name]).ServeHTTP(w, r)
}

// credential is the key a request carries, as the provider takes it.
func credential(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// proxy sends requests on to p, with key in place of theirs if not empty.
func (g *Gateway) proxy(p llmProvider, v VM, key string) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			up, _ := url.Parse(g.upstream[p.name])
			pr.SetURL(up)
			pr.Out.URL.Path = up.Path + strings.TrimPrefix(pr.In.URL.Path, "/"+p.name)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = up.Host
			// Plain answers, to read the tokens: the transport asks for
			// gzip itself and undoes it.
			pr.Out.Header.Del("Accept-Encoding")
			if key != "" {
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("X-Api-Key")
				if p.name == "anthropic" {
					pr.Out.Header.Set("X-Api-Key", key)
				} else {
					pr.Out.Header.Set("Authorization", "Bearer "+key)
				}
			}
			if p.name == "openai" && strings.HasSuffix(pr.Out.URL.Path, "/chat/completions") {
				askForUsage(pr.Out)
			}
		},
		FlushInterval: -1, // streams go on as they come
		ModifyResponse: func(resp *http.Response) error {
			ct := resp.Header.Get("Content-Type")
			if resp.StatusCode == http.StatusOK && (strings.HasPrefix(ct, "text/event-stream") || strings.HasPrefix(ct, "application/json")) {
				resp.Body = &meter{ReadCloser: resp.Body, sse: strings.HasPrefix(ct, "text/event-stream"),
					record: func(u llmUsage) { g.record(v, p.name, u) }}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "boxe gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// ownKeyMissing answers a request that needs the owner's key when there is none.
func ownKeyMissing(w http.ResponseWriter, p llmProvider) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	msg := fmt.Sprintf("no %s key in your boxe account: on your computer, run ssh boxe llm set %s", p.name, p.name)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "authentication_error", "message": msg}})
}

// askForUsage makes a streamed chat completion end with its tokens, which
// it leaves out unless asked.
// A larger body, an image-heavy request for example, goes on untouched.
func askForUsage(r *http.Request) {
	if r.Body == nil || r.ContentLength > requestLimit {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	if len(body) > requestLimit {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req map[string]json.RawMessage
	if err != nil || json.Unmarshal(body, &req) != nil || string(req["stream"]) != "true" {
		return
	}
	var opts map[string]json.RawMessage
	json.Unmarshal(req["stream_options"], &opts)
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	if _, set := opts["include_usage"]; set {
		return
	}
	opts["include_usage"] = json.RawMessage("true")
	req["stream_options"], _ = json.Marshal(opts)
	if body, err = json.Marshal(req); err != nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
}

// llmUsage is what one answer cost, in tokens. Input does not include
// the tokens read from or written to the cache.
type llmUsage struct {
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
}

// meter reads the tokens out of an answer as it passes through: the last
// usage object of a stream, or the one of a JSON answer.
type meter struct {
	io.ReadCloser
	sse    bool
	buf    []byte // the JSON answer, or the event line so far
	over   bool   // buf went past meterLimit
	usage  llmUsage
	seen   bool
	once   sync.Once
	record func(llmUsage)
}

// requestLimit is the largest request body the gateway reads (to ask for
// usage), meterLimit the largest JSON answer or event it reads the tokens
// of: what passes through is not limited.
const requestLimit, meterLimit = 4 << 20, 4 << 20

func (m *meter) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	if m.sse {
		m.lines(p[:n])
	} else if !m.over {
		m.buf = append(m.buf, p[:n]...)
		if len(m.buf) > meterLimit {
			m.over, m.buf = true, nil
		}
	}
	if err != nil {
		m.done()
	}
	return n, err
}

// lines parses the events in data, keeping a line cut in two for the next
// read. A line too long is skipped to its end.
func (m *meter) lines(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if !m.over {
				m.buf = append(m.buf, data...)
				if len(m.buf) > meterLimit {
					m.over, m.buf = true, nil
				}
			}
			return
		}
		if !m.over {
			m.buf = append(m.buf, data[:i]...)
			m.event(m.buf)
		}
		m.over, m.buf, data = false, m.buf[:0], data[i+1:]
	}
}

func (m *meter) event(line []byte) {
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
		m.parse(bytes.TrimSpace(data))
	}
}

func (m *meter) Close() error {
	m.done()
	return m.ReadCloser.Close()
}

func (m *meter) done() {
	m.once.Do(func() {
		switch {
		case m.over:
		case m.sse:
			m.event(m.buf)
		default:
			m.parse(m.buf)
		}
		if m.seen {
			m.record(m.usage)
		}
	})
}

// usageFields are the token counts of both providers' APIs.
type usageFields struct {
	InputTokens         *int64 `json:"input_tokens"`  // Anthropic, OpenAI Responses (with cached)
	OutputTokens        *int64 `json:"output_tokens"` // Anthropic, OpenAI Responses
	CacheRead           *int64 `json:"cache_read_input_tokens"`
	CacheWrite          *int64 `json:"cache_creation_input_tokens"`
	PromptTokens        *int64 `json:"prompt_tokens"` // OpenAI chat (with cached)
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type usageHolder struct {
	Model string       `json:"model"`
	Usage *usageFields `json:"usage"`
}

func (m *meter) parse(data []byte) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var ev struct {
		usageHolder
		Message  *usageHolder `json:"message"`  // Anthropic message_start
		Response *usageHolder `json:"response"` // OpenAI response.completed
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	for _, h := range []*usageHolder{&ev.usageHolder, ev.Message, ev.Response} {
		if h == nil {
			continue
		}
		if h.Model != "" {
			m.usage.Model = h.Model
		}
		if h.Usage != nil {
			m.add(h.Usage)
		}
	}
}

// add takes the counts in f: later ones in a stream are the totals so far.
func (m *meter) add(f *usageFields) {
	u := &m.usage
	set := func(to *int64, from *int64) {
		if from != nil {
			*to = *from
			m.seen = true
		}
	}
	set(&u.Output, f.OutputTokens)
	set(&u.Output, f.CompletionTokens)
	set(&u.CacheRead, f.CacheRead)
	set(&u.CacheWrite, f.CacheWrite)
	switch {
	case f.PromptTokens != nil: // OpenAI chat: the prompt counts the cached tokens too
		cached := int64(0)
		if f.PromptTokensDetails != nil {
			cached = f.PromptTokensDetails.Cached
		}
		u.Input, u.CacheRead, m.seen = *f.PromptTokens-cached, cached, true
	case f.InputTokensDetails != nil && f.InputTokens != nil: // OpenAI Responses, likewise
		u.Input, u.CacheRead, m.seen = *f.InputTokens-f.InputTokensDetails.Cached, f.InputTokensDetails.Cached, true
	default:
		set(&u.Input, f.InputTokens)
	}
}

func (g *Gateway) record(v VM, provider string, u llmUsage) {
	var cost *float64
	if p, ok := priceOf(u.Model); ok {
		c := (float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
		cost = &c
	}
	if err := g.store.AddLLMUsage(time.Now(), v.Owner, v.Name, provider, u, cost); err != nil {
		log.Printf("llm usage of %s: %v", v.Name, err)
	}
}

// check says whether the provider takes key.
func (g *Gateway) check(p llmProvider, key string) error {
	req, _ := http.NewRequest("GET", g.upstream[p.name]+p.models, nil)
	if p.name == "anthropic" {
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not check the key with %s: %v", p.name, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s refused the key", p.name)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not check the key with %s: %s", p.name, resp.Status)
	}
	return nil
}

// price is in USD per million tokens.
type price struct{ Input, Output, CacheRead, CacheWrite float64 }

// prices by model, list prices of the providers' own APIs. A dated
// snapshot (-20250929, -2025-08-07) costs as its model. Cache writes are
// priced as those that last 5 minutes; long-context and batch prices are
// not counted. Unknown models are counted without cost.
var prices = map[string]price{
	"claude-fable-5-1":       {10, 50, 0.25, 12.5},
	"claude-fable-5":         {10, 50, 1, 12.5},
	"claude-opus-5-5":        {4, 20, 0.20, 5},
	"claude-opus-5":          {5, 25, 0.50, 6.25},
	"claude-opus-4-8":        {5, 25, 0.50, 6.25},
	"claude-opus-4-7":        {5, 25, 0.50, 6.25},
	"claude-opus-4-6":        {5, 25, 0.50, 6.25},
	"claude-opus-4-5":        {5, 25, 0.50, 6.25},
	"claude-sonnet-5-5":      {2, 10, 0.20, 2.5},
	"claude-sonnet-5":        {2, 10, 0.20, 2.5},
	"claude-sonnet-4-6":      {3, 15, 0.30, 3.75},
	"claude-sonnet-4-5":      {3, 15, 0.30, 3.75},
	"claude-sonnet-4":        {3, 15, 0.30, 3.75},
	"claude-haiku-5-5":       {0.10, 0.50, 0.01, 0.125},
	"claude-haiku-4-5":       {1, 5, 0.10, 1.25},
	"gpt-5":                  {1.25, 10, 0.125, 0},
	"gpt-5-mini":             {0.25, 2, 0.025, 0},
	"gpt-5-nano":             {0.05, 0.40, 0.005, 0},
	"gpt-4.1":                {2, 8, 0.50, 0},
	"gpt-4.1-mini":           {0.40, 1.60, 0.10, 0},
	"gpt-4.1-nano":           {0.10, 0.40, 0.025, 0},
	"gpt-4o":                 {2.5, 10, 1.25, 0},
	"gpt-4o-mini":            {0.15, 0.60, 0.075, 0},
	"o3":                     {2, 8, 0.50, 0},
	"o4-mini":                {1.10, 4.40, 0.275, 0},
	"text-embedding-3-small": {0.02, 0, 0, 0},
	"text-embedding-3-large": {0.13, 0, 0, 0},
}

var snapshot = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

func priceOf(model string) (price, bool) {
	p, ok := prices[snapshot.ReplaceAllString(model, "")]
	return p, ok
}

// cmdLLM is `ssh boxe llm [set|rm|usage]`.
func (b *Box) cmdLLM(o output, ask func(string) (string, error), user string, args []string) error {
	if b.llm == nil {
		return fmt.Errorf("the LLM gateway is not on in this installation")
	}
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "":
		return b.llmList(o, user)
	case "set", "rm":
		if len(args) != 1 {
			return fmt.Errorf("usage: ssh %s llm %s <%s>", b.name, sub, providerNames())
		}
		p, ok := llmProviderNamed(args[0])
		if !ok {
			return fmt.Errorf("unknown provider %q: %s", args[0], providerNames())
		}
		before, err := b.store.LLMKeys(user)
		if err != nil {
			return err
		}
		if sub == "set" {
			key, err := ask(fmt.Sprintf("Paste your %s API key, then press Enter (with ssh -t it is not shown): ", p.name))
			if err != nil {
				return err
			}
			key = strings.TrimSpace(key)
			if key == "" || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
				return fmt.Errorf("that does not look like an API key")
			}
			if err := b.llm.check(p, key); err != nil {
				return err
			}
			if err := b.store.SetLLMKey(user, p.name, key); err != nil {
				return err
			}
		} else if err := b.store.DeleteLLMKey(user, p.name); err != nil {
			return err
		}
		after, err := b.store.LLMKeys(user)
		if err != nil {
			return err
		}
		if (len(before) == 0) != (len(after) == 0) {
			b.restartShelley(user)
		}
		status := map[bool]string{true: "saved", false: "removed"}[sub == "set"]
		text := fmt.Sprintf("%s key %s. Your VMs use it from the next login shell; Shelley already does.\n", p.name, status)
		if sub == "rm" {
			text = fmt.Sprintf("%s key removed.\n", p.name)
		}
		o.print(map[string]string{"provider": p.name, "status": status}, text)
		return nil
	case "usage":
		return b.llmUsage(o, user, args)
	}
	return fmt.Errorf("unknown llm command %q: ssh %s help", sub, b.name)
}

func providerNames() string {
	var names []string
	for _, p := range llmProviders {
		names = append(names, p.name)
	}
	return strings.Join(names, "|")
}

func (b *Box) llmList(o output, user string) error {
	keys, err := b.store.LLMKeys(user)
	if err != nil {
		return err
	}
	type item struct {
		Provider string `json:"provider"`
		Set      bool   `json:"set"`
		Key      string `json:"key,omitempty"` // its last characters
		BaseURL  string `json:"base_url"`
	}
	var list []item
	var text strings.Builder
	w := tabwriter.NewWriter(&text, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tKEY\tIN YOUR VMS")
	for _, p := range llmProviders {
		it := item{Provider: p.name, Set: keys[p.name] != "", BaseURL: b.llm.URL() + "/" + p.name}
		shown := "not set"
		if it.Set {
			k := keys[p.name]
			it.Key = "…" + k[max(0, len(k)-4):]
			shown = it.Key
		}
		list = append(list, it)
		fmt.Fprintf(w, "%s\t%s\t%s=%s\n", p.name, shown, p.baseEnv, it.BaseURL)
	}
	w.Flush()
	fmt.Fprintf(&text, "\nAdd or change a key: ssh %[1]s llm set <provider>; remove it: ssh %[1]s llm rm <provider>\n"+
		"The key stays on the host, which adds it to the requests of your VMs.\nWhat they used: ssh %[1]s llm usage\n", b.name)
	o.print(list, text.String())
	return nil
}

// llmUsageJSON is one VM's use of one model, as `llm usage --json` shows it.
type llmUsageJSON struct {
	VM         string   `json:"vm"`
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Requests   int64    `json:"requests"`
	Input      int64    `json:"input_tokens"`
	Output     int64    `json:"output_tokens"`
	CacheRead  int64    `json:"cache_read_tokens"`
	CacheWrite int64    `json:"cache_write_tokens"`
	CostUSD    *float64 `json:"cost_usd"` // null: the price of the model is not known
}

func (b *Box) llmUsage(o output, user string, args []string) error {
	days := 30
	if len(args) == 2 && args[0] == "--days" {
		if _, err := fmt.Sscan(args[1], &days); err != nil || days < 1 {
			return fmt.Errorf("--days like 7")
		}
	} else if len(args) > 0 {
		return fmt.Errorf("usage: ssh %s llm usage [--days 30]", b.name)
	}
	list, err := b.store.LLMUsage(user, time.Now().AddDate(0, 0, -days))
	if err != nil {
		return err
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].VM != list[j].VM {
			return list[i].VM < list[j].VM
		}
		return list[i].Model < list[j].Model
	})
	var text strings.Builder
	w := tabwriter.NewWriter(&text, 0, 0, 2, ' ', 0)
	fmt.Fprintf(&text, "Last %d days\n", days)
	fmt.Fprintln(w, "VM\tMODEL\tREQUESTS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tCOST")
	total, unknown := 0.0, false
	for _, u := range list {
		cost := "?"
		if u.CostUSD != nil {
			cost = fmt.Sprintf("$%.4f", *u.CostUSD)
			total += *u.CostUSD
		} else {
			unknown = true
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", u.VM, u.Model, u.Requests,
			tokens(u.Input), tokens(u.Output), tokens(u.CacheRead), tokens(u.CacheWrite), cost)
	}
	w.Flush()
	fmt.Fprintf(&text, "\nTotal: $%.2f", total)
	if unknown {
		text.WriteString(", plus models whose price boxe does not know (?)")
	}
	text.WriteString(". Estimated from list prices: your provider's bill is what counts.\n")
	if list == nil {
		list = []llmUsageJSON{}
	}
	o.print(list, text.String())
	return nil
}

// tokens is a count for people: 950, 12.3k, 4.1M.
func tokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

// restartShelley restarts Shelley in user's running VMs: it reads whether
// to use the gateway when it starts.
func (b *Box) restartShelley(user string) {
	vms, err := b.store.List(user)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, v := range vms {
		if !b.hv.Running(v.Name) || b.hv.Sleeping(v.Name) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := b.hv.inVM(v, 10*time.Second, "sudo systemctl try-restart shelley.service"); err != nil {
				log.Printf("restart Shelley in %s: %v: %s", v.Name, err, out)
			}
		}()
	}
	wg.Wait()
}

// readLine reads one line, the answer to ask, from a session without a terminal.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no key on the input")
	}
	return line, nil
}
