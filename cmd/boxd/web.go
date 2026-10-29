package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Web serves https://<vm>.<domain>, port 8000 of the VM, and
// https://<vm>--<port>.<domain> for ports 3000-9999, to members of the team
// logged in on the IdP. TLS ends at the proxy in front of boxd.
//
// VMs are untrusted, so each host has its own host-only session cookie that
// no other host can read or set. The login happens once on <domain>, which
// then hands a short-lived token to each VM host the user opens.
type Web struct {
	cfg    WebConfig
	store  *Store
	vmAddr func(v VM, port int) string
	vmLock *sync.RWMutex // Box.mu: VMs do not change while a connection to one opens
	now    func() time.Time
	health func() []string // problems for /healthz; nil: always ok

	mu  sync.Mutex
	idp struct{ Auth, Token, Userinfo string } // from discovery, on first use
}

type WebConfig struct {
	Domain                 string // e.g. box.example.com
	Issuer                 string // OIDC issuer, e.g. https://login.example.com
	ClientID, ClientSecret string
	OrgID                  string   // Zitadel organization whose users get in, or:
	EmailDomains           []string // users with a verified email in one of these domains
	Secret                 []byte   // signs session cookies and tokens
}

const (
	sessionCookie = "__Host-box_session"
	stateCookie   = "__Host-box_state"
	// Every session ends this long after the login at the IdP, wherever and
	// whenever it was handed on: who leaves the team loses access within it.
	authMaxAge = 12 * time.Hour
	vmWebPort  = 8000
	minVMPort  = 3000 // other ports: <vm>--<port>, 3000-9999
)

func NewWeb(cfg WebConfig, store *Store) *Web {
	w := &Web{cfg: cfg, store: store, now: time.Now}
	w.vmAddr = func(v VM, port int) string { return fmt.Sprintf("%s:%d", v.IP(), port) }
	return w
}

// endpoints runs OIDC discovery once it succeeds; until then every login
// tries again, so boxd starts even when the IdP is down.
func (w *Web) endpoints() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.idp.Auth != "" {
		return nil
	}
	req, _ := http.NewRequest("GET", strings.TrimSuffix(w.cfg.Issuer, "/")+"/.well-known/openid-configuration", nil)
	var d struct {
		Auth     string `json:"authorization_endpoint"`
		Token    string `json:"token_endpoint"`
		Userinfo string `json:"userinfo_endpoint"`
	}
	if err := doJSON(req, &d); err != nil {
		return fmt.Errorf("oidc discovery: %w", err)
	}
	if d.Auth == "" || d.Token == "" || d.Userinfo == "" {
		return fmt.Errorf("oidc discovery: incomplete configuration from %s", w.cfg.Issuer)
	}
	w.idp.Auth, w.idp.Token, w.idp.Userinfo = d.Auth, d.Token, d.Userinfo
	return nil
}

func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == w.cfg.Domain {
		switch r.URL.Path {
		case "/__box/login":
			w.login(rw, r)
		case "/oauth/callback":
			w.callback(rw, r)
		case "/healthz":
			var p []string
			if w.health != nil {
				p = w.health()
			}
			if len(p) > 0 {
				rw.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(rw, strings.Join(p, "\n")+"\n")
				return
			}
			io.WriteString(rw, "ok\n")
		case "/logout":
			if s, ok := w.session(r, w.cfg.Domain); ok {
				if err := w.store.Logout(s.E, w.now()); err != nil {
					log.Printf("web: logout %s: %v", s.E, err)
					http.Error(rw, "Could not log out, try again.", http.StatusInternalServerError)
					return
				}
				log.Printf("web: %s logged out", s.E)
			}
			http.SetCookie(rw, cookie(sessionCookie, "", -1))
			io.WriteString(rw, "Logged out of every VM.\n")
		default:
			io.WriteString(rw, "box: microVMs for the team.\nhttps://<vm>."+w.cfg.Domain+" shows port 8000 of the VM, https://<vm>--3000."+w.cfg.Domain+" port 3000 (3000-9999).\n")
		}
		return
	}
	name, port, ok := w.vmHost(host)
	if !ok {
		http.NotFound(rw, r)
		return
	}
	if r.URL.Path == "/__box/session" {
		w.acceptHandoff(rw, r, host)
		return
	}
	s, ok := w.session(r, host)
	if !ok {
		http.Redirect(rw, r, "https://"+w.cfg.Domain+"/__box/login?"+url.Values{"r": {"https://" + host + r.URL.RequestURI()}}.Encode(), http.StatusFound)
		return
	}
	v, err := w.store.Get(name)
	if err != nil {
		http.Error(rw, "There is no VM named "+name+".", http.StatusNotFound)
		return
	}
	w.proxy(v, port, s.E).ServeHTTP(rw, r)
}

// vmHost returns the VM and its port for a host like <vm>.<domain> (port
// 8000) or <vm>--<port>.<domain>. VM names cannot contain "--".
func (w *Web) vmHost(host string) (name string, port int, ok bool) {
	label, ok := strings.CutSuffix(host, "."+w.cfg.Domain)
	if !ok || strings.Contains(label, ".") {
		return "", 0, false
	}
	name, p, hasPort := strings.Cut(label, "--")
	if name == "" {
		return "", 0, false
	}
	if !hasPort {
		return name, vmWebPort, true
	}
	if len(p) != 4 || p[0] == '0' || strings.Trim(p, "0123456789") != "" {
		return "", 0, false
	}
	port, _ = strconv.Atoi(p)
	return name, port, port >= minVMPort
}

func (w *Web) session(r *http.Request, host string) (claims, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return claims{}, false
	}
	s, ok := w.verify(c.Value, "session")
	if !ok || s.H != host || s.E == "" {
		return claims{}, false
	}
	// Logging out (or an admin's revoke) ends every session from before.
	out, err := w.store.LogoutAt(s.E)
	return s, err == nil && s.A > out
}

// sessionCookie for email on host, ending authMaxAge after the login at
// the IdP (auth, in Unix milliseconds).
func (w *Web) sessionCookie(email, host string, auth int64) *http.Cookie {
	ttl := time.UnixMilli(auth).Add(authMaxAge).Sub(w.now())
	return cookie(sessionCookie, w.sign(claims{P: "session", E: email, H: host, A: auth}, ttl), int(ttl.Seconds()))
}

// login on <domain>: with a session there, hand off to the VM host right
// away; otherwise go to the IdP first.
func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	ret, err := url.Parse(r.FormValue("r"))
	if err != nil || ret.Scheme != "https" || ret.User != nil || ret.Port() != "" {
		http.Error(rw, "Invalid address.", http.StatusBadRequest)
		return
	}
	if _, _, ok := w.vmHost(ret.Host); !ok {
		http.Error(rw, "Invalid address.", http.StatusBadRequest)
		return
	}
	if s, ok := w.session(r, w.cfg.Domain); ok {
		w.handoff(rw, r, s, ret)
		return
	}
	if err := w.endpoints(); err != nil {
		log.Printf("web: %v", err)
		http.Error(rw, "The login service is not reachable, try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	nonce := randomString()
	http.SetCookie(rw, cookie(stateCookie, nonce, 600))
	q := url.Values{
		"client_id":     {w.cfg.ClientID},
		"redirect_uri":  {w.redirectURI()},
		"response_type": {"code"},
		"scope":         {"openid email profile urn:zitadel:iam:user:resourceowner"},
		"state":         {w.sign(claims{P: "state", R: ret.String(), N: nonce}, 10*time.Minute)},
	}
	http.Redirect(rw, r, w.idp.Auth+"?"+q.Encode(), http.StatusFound)
}

func (w *Web) callback(rw http.ResponseWriter, r *http.Request) {
	st, ok := w.verify(r.FormValue("state"), "state")
	c, err := r.Cookie(stateCookie)
	if !ok || err != nil || c.Value != st.N {
		http.Error(rw, "Login expired or invalid, open the page again.", http.StatusBadRequest)
		return
	}
	if err := w.endpoints(); err != nil {
		log.Printf("web: %v", err)
		http.Error(rw, "The login service is not reachable, try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	email, verified, org, err := w.userinfo(r.FormValue("code"))
	if err != nil {
		log.Printf("web: login: %v", err)
		http.Error(rw, "Login failed.", http.StatusBadGateway)
		return
	}
	if !w.member(email, verified, org) {
		log.Printf("web: login refused for %s (organization %s)", email, org)
		http.Error(rw, "This account is not part of the team.", http.StatusForbidden)
		return
	}
	log.Printf("web: %s logged in", email)
	http.SetCookie(rw, cookie(stateCookie, "", -1))
	s := claims{E: email, A: w.now().UnixMilli()}
	http.SetCookie(rw, w.sessionCookie(email, w.cfg.Domain, s.A))
	ret, _ := url.Parse(st.R) // checked in login, and signed since
	w.handoff(rw, r, s, ret)
}

// handoff sends the browser to the VM host with a token only that host accepts.
func (w *Web) handoff(rw http.ResponseWriter, r *http.Request, s claims, ret *url.URL) {
	path := ret.RequestURI()
	t := w.sign(claims{P: "handoff", E: s.E, A: s.A, H: ret.Host, R: path}, time.Minute)
	http.Redirect(rw, r, "https://"+ret.Host+"/__box/session?"+url.Values{"t": {t}}.Encode(), http.StatusFound)
}

func (w *Web) acceptHandoff(rw http.ResponseWriter, r *http.Request, host string) {
	t, ok := w.verify(r.FormValue("t"), "handoff")
	if !ok || t.H != host || t.E == "" || !strings.HasPrefix(t.R, "/") || strings.HasPrefix(t.R, "//") {
		http.Error(rw, "Login expired or invalid, open the page again.", http.StatusBadRequest)
		return
	}
	if out, err := w.store.LogoutAt(t.E); err != nil || t.A <= out {
		http.Error(rw, "Login expired or invalid, open the page again.", http.StatusBadRequest)
		return
	}
	http.SetCookie(rw, w.sessionCookie(t.E, host, t.A))
	http.Redirect(rw, r, t.R, http.StatusFound)
}

// userinfo exchanges the code and asks the IdP who logged in. Both calls go
// straight to the IdP over TLS, so the answer needs no further verification.
func (w *Web) userinfo(code string) (email string, verified bool, org string, err error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {w.redirectURI()}}
	req, _ := http.NewRequest("POST", w.idp.Token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(w.cfg.ClientID), url.QueryEscape(w.cfg.ClientSecret))
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := doJSON(req, &tok); err != nil {
		return "", false, "", fmt.Errorf("token: %w", err)
	}
	req, _ = http.NewRequest("GET", w.idp.Userinfo, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	var info struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Org      string `json:"urn:zitadel:iam:user:resourceowner:id"`
	}
	if err := doJSON(req, &info); err != nil {
		return "", false, "", fmt.Errorf("userinfo: %w", err)
	}
	return info.Email, info.Verified, info.Org, nil
}

// member says whether a login belongs to the team: by Zitadel organization
// if one is set, otherwise by a verified email in an allowed domain.
// With neither, nobody gets in.
func (w *Web) member(email string, verified bool, org string) bool {
	if email == "" {
		return false
	}
	if w.cfg.OrgID != "" {
		return org == w.cfg.OrgID
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, d := range w.cfg.EmailDomains {
		if verified && strings.EqualFold(domain, d) {
			return true
		}
	}
	return false
}

func doJSON(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// isBoxCookie tells the cookies of box apart from the app's own: the VM
// must neither read nor set them.
func isBoxCookie(name string) bool {
	name = strings.TrimPrefix(strings.TrimPrefix(name, "__Host-"), "__Secure-")
	return strings.HasPrefix(strings.ToLower(name), "box_")
}

// proxy forwards to the VM with the user's identity in X-Box-Email.
func (w *Web) proxy(v VM, port int, email string) http.Handler {
	target := &url.URL{Scheme: "http", Host: w.vmAddr(v, port)}
	// Connect only while v is still the VM at this address, as the SSH jump
	// does: a VM deleted meanwhile may have passed its address to another.
	// No connection reuse, which could outlive v.
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if w.vmLock != nil {
				w.vmLock.RLock()
				defer w.vmLock.RUnlock()
			}
			cur, err := w.store.Get(v.Name)
			if err != nil || cur.Index != v.Index || !cur.Created.Equal(v.Created) {
				return nil, fmt.Errorf("%s is gone", v.Name)
			}
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetURL(target)
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			for k := range pr.Out.Header {
				if strings.HasPrefix(k, "X-Box-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Set("X-Box-Email", email)
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if !isBoxCookie(c.Name) {
					pr.Out.AddCookie(c)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			var keep []string
			for _, line := range resp.Header.Values("Set-Cookie") {
				if c, err := http.ParseSetCookie(line); err == nil && !isBoxCookie(c.Name) {
					keep = append(keep, line)
				}
			}
			resp.Header.Del("Set-Cookie")
			for _, line := range keep {
				resp.Header.Add("Set-Cookie", line)
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			http.Error(rw, fmt.Sprintf("%s is not answering on port %d.", v.Name, port), http.StatusBadGateway)
		},
	}
}

func (w *Web) redirectURI() string { return "https://" + w.cfg.Domain + "/oauth/callback" }

// cookie is host-only: __Host- cookies cannot be set by another host.
func cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// claims of the signed tokens. P is the purpose: a token made for one
// purpose (state, handoff, session) is never accepted for another.
type claims struct {
	P string `json:"p"`
	X int64  `json:"x"`
	E string `json:"e,omitempty"` // email
	H string `json:"h,omitempty"` // host the token is for
	R string `json:"r,omitempty"` // where to go next
	N string `json:"n,omitempty"` // nonce, also in the state cookie
	A int64  `json:"a,omitempty"` // login at the IdP, Unix milliseconds
}

// sign returns base64(json(claims)).base64(hmac).
func (w *Web) sign(c claims, ttl time.Duration) string {
	c.X = w.now().Add(ttl).Unix()
	b, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(w.mac(p))
}

func (w *Web) verify(s, purpose string) (claims, bool) {
	var c claims
	p, sig, ok := strings.Cut(s, ".")
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(got, w.mac(p)) {
		return c, false
	}
	b, _ := base64.RawURLEncoding.DecodeString(p)
	if json.Unmarshal(b, &c) != nil || c.P != purpose || w.now().Unix() > c.X {
		return claims{}, false
	}
	return c, true
}

func (w *Web) mac(p string) []byte {
	m := hmac.New(sha256.New, w.cfg.Secret)
	m.Write([]byte(p))
	return m.Sum(nil)
}

func randomString() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
