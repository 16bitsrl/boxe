package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testOrg = "org-team"

// fakeIdP stands in for Zitadel: code "member" logs in a team member,
// code "outsider" a user of another organization. Until up is set, discovery
// fails as if the IdP were down.
type fakeIdP struct {
	*httptest.Server
	up         atomic.Bool
	authorizes atomic.Int32
}

func newFakeIdP(t *testing.T) *fakeIdP {
	idp := &fakeIdP{}
	idp.up.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !idp.up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.URL,
			"authorization_endpoint": idp.URL + "/authorize",
			"token_endpoint":         idp.URL + "/token",
			"userinfo_endpoint":      idp.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if id, secret, _ := r.BasicAuth(); id != "client" || secret != "secret" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + r.Form.Get("code"), "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		org := testOrg
		if r.Header.Get("Authorization") == "Bearer tok-outsider" {
			org = "org-other"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"email":                                 "alice@example.com",
			"email_verified":                        true,
			"urn:zitadel:iam:user:resourceowner:id": org,
		})
	})
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

type webTest struct {
	t       *testing.T
	web     *Web
	idp     *fakeIdP
	backend *httptest.Server
	seen    *http.Request // last request that reached a VM
	port    int           // VM port of the last request
	reply   func(w http.ResponseWriter)
}

func newWebTest(t *testing.T) *webTest {
	wt := &webTest{t: t, idp: newFakeIdP(t)}
	wt.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wt.seen = r
		if wt.reply != nil {
			wt.reply(w)
		}
		io.WriteString(w, "hello from "+r.Host)
	}))
	t.Cleanup(wt.backend.Close)
	store, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test", "other"} {
		if err := store.Insert(&VM{Name: name, Owner: "alice", CPUs: 1, Mem: "1G"}); err != nil {
			t.Fatal(err)
		}
	}
	wt.web = NewWeb(WebConfig{
		Domain:       "boxe.example",
		Issuer:       wt.idp.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		OrgID:        testOrg,
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
	}, store)
	wt.web.vmAddr = func(_ VM, port int) string {
		wt.port = port
		return strings.TrimPrefix(wt.backend.URL, "http://")
	}
	return wt
}

// browser keeps cookies per host like a real one and follows redirects. At
// the IdP it logs in with code, as if the user had typed their password.
type browser struct {
	wt   *webTest
	jar  *cookiejar.Jar
	code string
	set  []*http.Cookie // every cookie boxed asked to set
}

func (wt *webTest) browser(code string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{wt: wt, jar: jar, code: code}
}

// do sends one GET to boxed, without following redirects.
func (b *browser) do(rawURL string, header http.Header) *http.Response {
	return b.send("GET", rawURL, header)
}

func (b *browser) send(method, rawURL string, header http.Header) *http.Response {
	u, _ := url.Parse(rawURL)
	r := httptest.NewRequest(method, u.RequestURI(), nil)
	r.Host = u.Host
	for k, v := range header {
		r.Header[k] = v
	}
	for _, c := range b.jar.Cookies(u) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.wt.web.ServeHTTP(w, r)
	res := w.Result()
	b.set = append(b.set, res.Cookies()...)
	b.jar.SetCookies(u, res.Cookies())
	return res
}

// get follows redirects, through the IdP, until a page that is not a redirect.
func (b *browser) get(rawURL string, header http.Header) *http.Response {
	for range 10 {
		res := b.do(rawURL, header)
		if res.StatusCode != http.StatusFound {
			return res
		}
		loc, _ := url.Parse(res.Header.Get("Location"))
		if base, _ := url.Parse(rawURL); !loc.IsAbs() {
			loc = base.ResolveReference(loc)
		}
		if strings.HasPrefix(loc.String(), b.wt.idp.URL+"/authorize") {
			b.wt.idp.authorizes.Add(1)
			q := loc.Query()
			if q.Get("client_id") != "client" || q.Get("redirect_uri") != "https://boxe.example/oauth/callback" {
				b.wt.t.Fatalf("authorize query %v", q)
			}
			loc, _ = url.Parse(q.Get("redirect_uri") + "?" + url.Values{"code": {b.code}, "state": {q.Get("state")}}.Encode())
		}
		rawURL = loc.String()
	}
	b.wt.t.Fatal("too many redirects")
	return nil
}

func body(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestWebLoginAndProxy(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.jar.SetCookies(&url.URL{Scheme: "https", Host: "test.boxe.example"}, []*http.Cookie{{Name: "app", Value: "mine"}})

	res := b.get("https://test.boxe.example/app?x=1", http.Header{"X-Box-Email": {"boss@example.com"}})
	if res.StatusCode != 200 || body(res) != "hello from test.boxe.example" {
		t.Fatalf("after login: %d", res.StatusCode)
	}
	r := wt.seen
	if r.URL.RequestURI() != "/app?x=1" {
		t.Errorf("VM got %s, want the page asked for", r.URL.RequestURI())
	}
	if got := r.Header.Get("X-Box-Email"); got != "alice@example.com" {
		t.Errorf("X-Box-Email %q, want the logged-in user", got)
	}
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("X-Forwarded-Proto %q", r.Header.Get("X-Forwarded-Proto"))
	}
	for _, c := range r.Cookies() {
		if strings.Contains(c.Name, "boxe_") {
			t.Errorf("the VM received the boxe cookie %s", c.Name)
		}
	}
	if c, err := r.Cookie("app"); err != nil || c.Value != "mine" {
		t.Error("the app's own cookie was not passed on")
	}

	// Another VM: no new login at the IdP.
	res = b.get("https://other.boxe.example/", nil)
	if res.StatusCode != 200 || body(res) != "hello from other.boxe.example" {
		t.Fatalf("second VM: %d", res.StatusCode)
	}
	if n := wt.idp.authorizes.Load(); n != 1 {
		t.Errorf("%d logins at the IdP, want 1", n)
	}

	if res := b.get("https://nope.boxe.example/", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown VM: %d, want 404", res.StatusCode)
	}
}

func TestWebSessionCookiesAreHostOnly(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	if len(b.set) < 3 {
		t.Fatalf("cookies set: %v, want state and two sessions", b.set)
	}
	for _, c := range b.set {
		if c.Domain != "" || !strings.HasPrefix(c.Name, "__Host-") || !c.Secure || !c.HttpOnly {
			t.Errorf("cookie %+v: want host-only __Host- cookie, Secure, HttpOnly", c)
		}
	}
}

func TestWebRejectsOtherOrganizations(t *testing.T) {
	wt := newWebTest(t)
	res := wt.browser("outsider").get("https://test.boxe.example/", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider: %d, want 403", res.StatusCode)
	}
}

func TestWebStateIsNotASession(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	res := b.do("https://boxe.example/__boxe/login?r="+url.QueryEscape("https://test.boxe.example/"), nil)
	loc, _ := url.Parse(res.Header.Get("Location"))
	state := loc.Query().Get("state")
	for _, name := range []string{"__Host-boxe_session", "boxe_session"} {
		evil := wt.browser("member")
		evil.jar.SetCookies(&url.URL{Scheme: "https", Host: "test.boxe.example"}, []*http.Cookie{{Name: name, Value: state}})
		if res := evil.do("https://test.boxe.example/", nil); res.StatusCode != http.StatusFound || wt.seen != nil {
			t.Errorf("state as %s: %d, the request reached the VM: %v", name, res.StatusCode, wt.seen != nil)
		}
	}
}

func TestWebHandoffIsBoundToItsHost(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.get("https://boxe.example/__boxe/login?r="+url.QueryEscape("https://test.boxe.example/"), nil)
	// Logged in on boxe.example: the next login hands off without the IdP.
	res := b.do("https://boxe.example/__boxe/login?r="+url.QueryEscape("https://test.boxe.example/"), nil)
	handoff, _ := url.Parse(res.Header.Get("Location"))
	if handoff.Host != "test.boxe.example" {
		t.Fatalf("handoff to %s", handoff)
	}
	handoff.Host = "other.boxe.example"
	evil := wt.browser("member")
	if res := evil.do(handoff.String(), nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("handoff on another host: %d, want 400", res.StatusCode)
	}
}

func TestWebLoginReturnsOnlyToVMs(t *testing.T) {
	wt := newWebTest(t)
	for _, r := range []string{"https://evil.com/", "https://boxe.example.evil.com/", "//evil.com/", "http://test.boxe.example/"} {
		res := wt.browser("member").do("https://boxe.example/__boxe/login?r="+url.QueryEscape(r), nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("return to %s: %d, want 400", r, res.StatusCode)
		}
	}
}

func TestWebVMCannotSetBoxCookies(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.get("https://test.boxe.example/", nil)
	wt.reply = func(w http.ResponseWriter) {
		w.Header().Add("Set-Cookie", "__Host-boxe_session=evil; Path=/; Secure; HttpOnly")
		w.Header().Add("Set-Cookie", "boxe_session=evil; Domain=boxe.example; Path=/")
		w.Header().Add("Set-Cookie", "app=1; Path=/")
	}
	res := b.do("https://test.boxe.example/", nil)
	var names []string
	for _, c := range res.Cookies() {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "app" {
		t.Errorf("cookies set by the VM: %v, want only app", names)
	}
}

func TestWebRejectsForgedStateAndCookies(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	if res := b.do("https://boxe.example/oauth/callback?code=member&state=forged", nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("forged state: %d, want 400", res.StatusCode)
	}
	b.jar.SetCookies(&url.URL{Scheme: "https", Host: "test.boxe.example"}, []*http.Cookie{{Name: "__Host-boxe_session", Value: "eyJlIjoibXRAMTZiaXQuaXQifQ.AAAA"}})
	if res := b.do("https://test.boxe.example/", nil); res.StatusCode != http.StatusFound {
		t.Errorf("forged session: %d, want redirect to login", res.StatusCode)
	}
}

func TestWebVMDown(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.get("https://test.boxe.example/", nil)
	wt.backend.Close()
	if res := b.do("https://test.boxe.example/", nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("VM not answering: %d, want 502", res.StatusCode)
	}
}

func TestWebIdPDownAtStart(t *testing.T) {
	wt := newWebTest(t)
	wt.idp.up.Store(false)
	if res := wt.browser("member").get("https://test.boxe.example/", nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("IdP down: %d, want 503", res.StatusCode)
	}
	wt.idp.up.Store(true)
	if res := wt.browser("member").get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Errorf("IdP back: %d, want 200", res.StatusCode)
	}
}

// <vm>--<port>.<domain> reaches the other ports of the VM, 3000-9999.
func TestWebOtherPorts(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != 200 || wt.port != 8000 {
		t.Fatalf("default: %d, port %d, want 200 on 8000", res.StatusCode, wt.port)
	}
	res := b.get("https://test--3000.boxe.example/x", nil)
	if res.StatusCode != 200 || wt.port != 3000 {
		t.Fatalf("test--3000: %d, port %d, want 200 on 3000", res.StatusCode, wt.port)
	}
	if got := wt.seen.Header.Get("X-Box-Email"); got != "alice@example.com" {
		t.Errorf("X-Box-Email %q", got)
	}
	if wt.seen.Host != "test--3000.boxe.example" {
		t.Errorf("Host %q", wt.seen.Host)
	}
	if n := wt.idp.authorizes.Load(); n != 1 {
		t.Errorf("%d logins at the IdP, want 1", n)
	}
	for _, host := range []string{"test--80", "test--10000", "test--2999", "test--abc", "test--03000", "test--", "--3000", "test--3000--4000"} {
		if res := b.do("https://"+host+".boxe.example/", nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", host, res.StatusCode)
		}
	}
	if res := b.do("https://boxe.example/__boxe/login?r="+url.QueryEscape("https://test--80.boxe.example/"), nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("login to test--80: %d, want 400", res.StatusCode)
	}
	wt.backend.Close()
	if res := b.do("https://test--3000.boxe.example/", nil); res.StatusCode != http.StatusBadGateway || !strings.Contains(body(res), "port 3000") {
		t.Errorf("VM down: %d, want 502 naming port 3000", res.StatusCode)
	}
}

// A VM deleted while a request is on its way, its address given to another
// VM: the request must not reach the new one.
func TestWebDoesNotReachAReplacedVM(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.get("https://test.boxe.example/", nil)
	store := wt.web.store
	addr := wt.web.vmAddr
	wt.web.vmAddr = func(v VM, port int) string {
		// Between lookup and connection: test is deleted, its index reused.
		store.Delete("test")
		store.Insert(&VM{Name: "newcomer", Owner: "someone", CPUs: 1, Mem: "1G"})
		return addr(v, port)
	}
	wt.seen = nil
	res := b.do("https://test.boxe.example/", nil)
	if wt.seen != nil {
		t.Fatal("the request reached the VM that took the address")
	}
	if res.StatusCode != http.StatusBadGateway && res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 502 or 404", res.StatusCode)
	}
}

// Logging out ends the sessions on every VM host, not only on <domain>.
func TestWebLogoutEndsEverySession(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	for _, h := range []string{"test", "other"} {
		if res := b.get("https://"+h+".boxe.example/", nil); res.StatusCode != 200 {
			t.Fatalf("%s: %d", h, res.StatusCode)
		}
	}
	time.Sleep(2 * time.Millisecond)
	if res := b.send("POST", "https://boxe.example/logout", http.Header{"Sec-Fetch-Site": {"same-origin"}}); res.StatusCode != 200 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	for _, h := range []string{"test", "other"} {
		if res := b.do("https://"+h+".boxe.example/", nil); res.StatusCode != http.StatusFound {
			t.Errorf("%s after logout: %d, want a redirect to login", h, res.StatusCode)
		}
	}
	// A new login works again.
	time.Sleep(2 * time.Millisecond)
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Errorf("login after logout: %d", res.StatusCode)
	}
}

// A VM's page must not log members out: an <img src=".../logout"> sends a
// GET, a form or fetch on a VM's host sends a cross-origin POST.
func TestWebLogoutFromAVMPageDoesNothing(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	time.Sleep(2 * time.Millisecond)
	b.do("https://boxe.example/logout", http.Header{"Sec-Fetch-Site": {"same-site"}})
	if res := b.send("POST", "https://boxe.example/logout", http.Header{
		"Sec-Fetch-Site": {"same-site"}, "Origin": {"https://test.boxe.example"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d, want 403", res.StatusCode)
	}
	if res := b.do("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Errorf("session ended by a VM's page: %d", res.StatusCode)
	}
	// Opened by hand, the page asks for a click.
	res := b.do("https://boxe.example/logout", http.Header{"Sec-Fetch-Site": {"none"}})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), `method="post"`) {
		t.Errorf("logout page: %d %q", res.StatusCode, body)
	}
}

// An admin can end someone's sessions (offboarding) without their browser.
func TestWebRevokeUser(t *testing.T) {
	wt := newWebTest(t)
	b := wt.browser("member")
	b.get("https://test.boxe.example/", nil)
	time.Sleep(2 * time.Millisecond)
	if err := wt.web.store.Logout("alice@example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	if res := b.do("https://test.boxe.example/", nil); res.StatusCode != http.StatusFound {
		t.Errorf("after revoke: %d, want a redirect to login", res.StatusCode)
	}
}

// Sessions end a fixed time after the login at the IdP: a VM host opened
// later does not get a session that outlives it.
func TestWebSessionsEndAfterLogin(t *testing.T) {
	wt := newWebTest(t)
	now := time.Now()
	wt.web.now = func() time.Time { return now }
	b := wt.browser("member")
	b.get("https://test.boxe.example/", nil)
	now = now.Add(authMaxAge - time.Hour)
	if res := b.get("https://other.boxe.example/", nil); res.StatusCode != 200 || wt.idp.authorizes.Load() != 1 {
		t.Fatalf("before expiry: %d, %d logins", res.StatusCode, wt.idp.authorizes.Load())
	}
	now = now.Add(2 * time.Hour)
	if res := b.do("https://other.boxe.example/", nil); res.StatusCode != http.StatusFound {
		t.Errorf("after expiry: %d, want a redirect to login", res.StatusCode)
	}
}

func TestWebHealth(t *testing.T) {
	wt := newWebTest(t)
	var problems []string
	wt.web.health = func() []string { return problems }
	res := wt.browser("x").do("https://boxe.example/healthz", nil)
	if res.StatusCode != 200 || body(res) != "ok\n" {
		t.Errorf("healthy: %d", res.StatusCode)
	}
	problems = []string{"backup: last one 40h ago"}
	res = wt.browser("x").do("https://boxe.example/healthz", nil)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body(res), "backup") {
		t.Errorf("unhealthy: %d", res.StatusCode)
	}
}

// Without a Zitadel organization, a list of email domains decides who gets in.
func TestWebEmailDomains(t *testing.T) {
	wt := newWebTest(t)
	wt.web.cfg.OrgID = ""
	wt.web.cfg.EmailDomains = []string{"example.com"}
	if res := wt.browser("member").get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Errorf("allowed domain: %d", res.StatusCode)
	}
	wt.web.cfg.EmailDomains = []string{"example.org"}
	if res := wt.browser("member").get("https://test.boxe.example/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("other domain: %d, want 403", res.StatusCode)
	}
	wt.web.cfg.EmailDomains = nil
	if res := wt.browser("member").get("https://test.boxe.example/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("no rule at all: %d, want 403 (nobody gets in by default)", res.StatusCode)
	}
}

// Opening a sleeping VM's page wakes it up first; while a request is open
// the VM counts as in use.
func TestWebWakesTheVM(t *testing.T) {
	wt := newWebTest(t)
	var woken []string
	inUse := map[string]int{}
	wt.web.wake = func(name string) error { woken = append(woken, name); return nil }
	wt.web.busy = func(name string, d int) { inUse[name] += d }
	wt.reply = func(http.ResponseWriter) {
		if inUse["test"] != 1 {
			t.Errorf("during the request the VM is in use %d times, want 1", inUse["test"])
		}
	}
	b := wt.browser("member")
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if len(woken) != 1 || woken[0] != "test" {
		t.Errorf("woken %v", woken)
	}
	if inUse["test"] != 0 {
		t.Errorf("still in use after the request: %d", inUse["test"])
	}

	wt.web.wake = func(string) error { return errors.New("could not wake test up") }
	if res := b.get("https://test.boxe.example/", nil); res.StatusCode != http.StatusBadGateway || !strings.Contains(body(res), "could not wake") {
		t.Errorf("failed wake: %d", res.StatusCode)
	}
}
