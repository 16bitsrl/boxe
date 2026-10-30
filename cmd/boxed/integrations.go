package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Integrations keep a member's API secrets out of their VMs. A VM calls
// http://<integration>.int.<suffix>/<path>; boxed answers on the gateway
// (DNS in dns.go points every such name there), knows the VM by its source
// address (net-setup.sh lets a tap send only with its own) and, if the
// integration is attached to it, forwards the request to the target with
// the secret added. The VM never sees the secret.

const (
	kindHTTP   = "http"
	kindGitHub = "github"
	// gatewayIP is the host on br0 (net-setup.sh): the integrations proxy
	// and the DNS server listen there.
	gatewayIP = "10.42.0.1"

	egressAll          = "all"
	egressIntegrations = "integrations" // only the gateway: integrations and their DNS names
)

// integrationJSON is an integration as `integrations --json` shows it: no secret.
type integrationJSON struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Target   string   `json:"target"`
	Header   string   `json:"header,omitempty"` // the name of the header added
	ReadOnly bool     `json:"read_only,omitempty"`
	URL      string   `json:"url"` // what VMs call
	VMs      []string `json:"vms"`
}

func (b *Box) integrationJSON(ig Integration) integrationJSON {
	j := integrationJSON{Name: ig.Name, Kind: ig.Kind, Target: shownTarget(ig), Header: ig.HeaderName, ReadOnly: ig.ReadOnly,
		URL: "http://" + ig.Name + ".int." + b.hv.SSHSuffix + "/", VMs: ig.VMs}
	if ig.Kind == kindGitHub {
		j.URL = "http://github.int." + b.hv.SSHSuffix + "/"
	}
	if j.VMs == nil {
		j.VMs = []string{}
	}
	return j
}

// shownTarget is the target without what may be a secret: with no header,
// the URL's path is the secret (a Slack webhook), so only its origin shows.
func shownTarget(ig Integration) string {
	if ig.Kind == kindGitHub {
		return "https://github.com"
	}
	u, err := url.Parse(ig.Target)
	if err != nil {
		return "-"
	}
	if ig.HeaderName == "" && strings.Trim(u.Path, "/") != "" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return ig.Target
}

func (b *Box) cmdIntegrations(o output, secret func(prompt string) (string, error), user string, args []string) error {
	usage := fmt.Errorf("usage: ssh %[1]s integrations [add http <name> --target <url> [--header 'Name: value'] | add github <name> --token <token> [--read-only] | rm <name> | attach <name> <vm> | detach <name> <vm>]", b.name)
	if len(args) == 0 || (len(args) == 1 && args[0] == "ls") {
		return b.listIntegrations(o, user)
	}
	switch {
	case args[0] == "add" && len(args) >= 3:
		return b.addIntegration(o, secret, user, args[1], args[2], args[3:])
	case args[0] == "rm" && len(args) == 2:
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, err := b.ownIntegration(user, args[1]); err != nil {
			return err
		}
		if err := b.store.RemoveIntegration(user, args[1]); err != nil {
			return err
		}
		log.Printf("%s: removed integration %s", user, args[1])
		o.print(done{args[1], "deleted"}, args[1]+" deleted\n")
		return nil
	case (args[0] == "attach" || args[0] == "detach") && len(args) == 3:
		return b.attach(o, user, args[1], strings.TrimPrefix(args[2], "vm:"), args[0] == "attach")
	}
	return usage
}

func (b *Box) ownIntegration(user, name string) (Integration, error) {
	ig, err := b.store.Integration(user, name)
	if err == errNotFound {
		return ig, fmt.Errorf("you have no integration named %s (ssh %s integrations lists them)", name, b.name)
	}
	return ig, err
}

func (b *Box) listIntegrations(o output, user string) error {
	list, err := b.store.Integrations(user)
	if err != nil {
		return err
	}
	js := []integrationJSON{}
	for _, ig := range list {
		js = append(js, b.integrationJSON(ig))
	}
	if o.json {
		o.print(js, "")
		return nil
	}
	if len(js) == 0 {
		fmt.Fprintf(o.w, "No integrations yet. Add one: ssh %s integrations add http <name> --target <url> --header 'Name: value'\n", b.name)
		return nil
	}
	w := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tTARGET\tAUTH\tVMS\tIN THE VM")
	for _, j := range js {
		auth := "none"
		switch {
		case j.Kind == kindGitHub && j.ReadOnly:
			auth = "token, read-only"
		case j.Kind == kindGitHub:
			auth = "token"
		case j.Header != "":
			auth = j.Header
		}
		vms := strings.Join(j.VMs, ", ")
		if vms == "" {
			vms = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Name, j.Kind, j.Target, auth, vms, j.URL)
	}
	return w.Flush()
}

var headerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// parseHeader reads "Name: value", a header the proxy may add.
func parseHeader(s string) (name, value string, err error) {
	name, value, ok := strings.Cut(s, ":")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	bad := !ok || !headerName.MatchString(name) || value == "" || strings.ContainsAny(value, "\r\n")
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Connection", "Content-Length", "Transfer-Encoding", "Te", "Trailer", "Upgrade", "Keep-Alive",
		"Proxy-Connection", "Proxy-Authorization":
		bad = true
	}
	if bad {
		return "", "", errors.New("invalid --header: like 'Authorization: Bearer <key>'")
	}
	return http.CanonicalHeaderKey(name), value, nil
}

// parseTarget checks an http integration's URL.
func parseTarget(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid --target: a URL like https://api.example.com or https://api.example.com/v1, with no query")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func (b *Box) addIntegration(o output, secret func(string) (string, error), user, kind, name string, args []string) error {
	if kind == "http-proxy" {
		kind = kindHTTP // as exe.dev calls it
	}
	if kind != kindHTTP && kind != kindGitHub {
		return fmt.Errorf("unknown kind %s: http or github", kind)
	}
	if err := checkName(name); err != nil {
		return err
	}
	if name == "github" && kind != kindGitHub {
		return fmt.Errorf("github is the name of the GitHub integration: choose another")
	}
	flags := map[string]string{}
	readOnly := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			a = "-" + strings.TrimLeft(a, "-") // --target or -target
		}
		switch a {
		case "-read-only":
			readOnly = true
		case "-target", "-header", "-token":
			if i+1 == len(args) {
				return fmt.Errorf("-%s needs a value (- reads it from stdin)", a)
			}
			if args[i+1] == "-" {
				what := map[string]string{"-target": "URL", "-header": "header (Name: value)", "-token": "GitHub token"}[a]
				v, err := secret(what + ": ")
				if err != nil {
					return fmt.Errorf("could not read the %s: %v", what, err)
				}
				args[i+1] = strings.TrimSpace(v)
			}
			flags[a] = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown option %s", args[i])
		}
	}
	ig := Integration{Owner: user, Name: name, Kind: kind, ReadOnly: readOnly}
	var err error
	if kind == kindHTTP {
		if _, ok := flags["-token"]; ok || readOnly {
			return fmt.Errorf("--token and --read-only are for github integrations")
		}
		if ig.Target, err = parseTarget(flags["-target"]); err != nil {
			return err
		}
		if h, ok := flags["-header"]; ok {
			if ig.HeaderName, ig.Secret, err = parseHeader(h); err != nil {
				return err
			}
		}
	} else {
		if _, ok := flags["-target"]; ok {
			return fmt.Errorf("a github integration has no --target")
		}
		if _, ok := flags["-header"]; ok {
			return fmt.Errorf("a github integration has no --header: --token")
		}
		ig.Secret = flags["-token"]
		if ig.Secret == "" || strings.ContainsAny(ig.Secret, " \t\r\n:") {
			return fmt.Errorf("a github integration needs --token <token> (- reads it from stdin): a fine-grained personal access token")
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.store.Integration(user, name); err == nil {
		return fmt.Errorf("you already have an integration named %s", name)
	}
	if err := b.store.AddIntegration(ig); err != nil {
		return err
	}
	log.Printf("%s: added integration %s (%s)", user, name, kind)
	j := b.integrationJSON(ig)
	text := fmt.Sprintf("%s added. Attach it to a VM: ssh %s integrations attach %s <vm>\n\nIn the VM: %s<path>\n", name, b.name, name, j.URL)
	if kind == kindGitHub {
		text = fmt.Sprintf("%s added. Attach it to a VM: ssh %s integrations attach %s <vm>\n\nIn the VM: git clone %sOWNER/REPO, and the API at %sapi/\n", name, b.name, name, j.URL, j.URL)
	}
	o.print(j, text)
	return nil
}

func (b *Box) attach(o output, user, name, vm string, on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ig, err := b.ownIntegration(user, name)
	if err != nil {
		return err
	}
	v, err := b.owned(user, vm)
	if err != nil {
		return err
	}
	if !on {
		if err := b.store.Detach(v, name); err != nil {
			return err
		}
		log.Printf("%s: detached %s from %s", user, name, v.Name)
		o.print(done{v.Name, "detached"}, fmt.Sprintf("%s no longer reaches %s\n", v.Name, name))
		return nil
	}
	// git and the API know one token per VM: the one at github.int.
	if ig.Kind == kindGitHub {
		if cur, err := b.store.Attached(v, "github"); err == nil && cur.Name != name {
			return fmt.Errorf("%s already has the GitHub integration %s: detach it first (ssh %s integrations detach %s %s)", v.Name, cur.Name, b.name, cur.Name, v.Name)
		}
	}
	if err := b.store.Attach(v, name); err != nil {
		return err
	}
	log.Printf("%s: attached %s to %s", user, name, v.Name)
	o.print(done{v.Name, "attached"}, fmt.Sprintf("%s reaches %s at %s\n", v.Name, name, b.integrationJSON(ig).URL))
	return nil
}

func (b *Box) cmdEgress(o output, user string, args []string) error {
	if len(args) != 2 || (args[1] != egressAll && args[1] != egressIntegrations) {
		return fmt.Errorf("usage: ssh %s egress <name> all|integrations", b.name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	v.Egress = args[1]
	// Closed first, then recorded; opened only once recorded: a failure
	// leaves it closed rather than open.
	if v.Egress == egressIntegrations {
		if err := b.hv.egress(v); err != nil {
			log.Printf("%s: egress %s: %v", user, v.Name, err)
			return fmt.Errorf("could not restrict %s", v.Name)
		}
	}
	if err := b.store.SetEgress(v.Name, v.Egress); err != nil {
		return err
	}
	if v.Egress == egressAll {
		b.hv.egress(v)
	}
	log.Printf("%s: egress of %s: %s", user, v.Name, v.Egress)
	text := v.Name + " reaches the internet\n"
	if v.Egress == egressIntegrations {
		text = v.Name + " reaches only its integrations (ssh " + b.name + " integrations)\n"
	}
	o.print(b.vmJSON(v), text)
	return nil
}

// vmAt is the index of the VM whose address is ip, if ip is on br0.
func vmAt(ip netip.Addr) (int, bool) {
	ip = ip.Unmap()
	if !netip.MustParsePrefix("10.42.0.0/16").Contains(ip) {
		return 0, false
	}
	a := ip.As4()
	return int(a[2])<<8 | int(a[3]), true
}

// caller is the VM at the source address of a request. Called with b.mu held.
func (b *Box) caller(remote string) (VM, error) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return VM{}, errNotFound
	}
	idx, ok := vmAt(ap.Addr())
	if !ok {
		return VM{}, errNotFound
	}
	return b.store.GetByIndex(idx)
}

// Proxy serves http://<integration>.int.<suffix> to the VMs.
type Proxy struct {
	b                    *Box
	transport            http.RoundTripper
	githubURL, githubAPI string
}

func (b *Box) proxy() *Proxy {
	return &Proxy{b: b, transport: publicTransport(), githubURL: "https://github.com", githubAPI: "https://api.github.com"}
}

type connVMKey struct{}

func (p *Proxy) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: log.New(io.Discard, "", 0),
		// A connection is the VM that opened it: another VM that takes its
		// address later is not served on it.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			p.b.mu.RLock()
			v, _ := p.b.caller(c.RemoteAddr().String())
			p.b.mu.RUnlock()
			return context.WithValue(ctx, connVMKey{}, v)
		}}
	return srv.Serve(ln)
}

// The same answer whether the integration exists or not.
func (p *Proxy) forbid(w http.ResponseWriter) {
	http.Error(w, "this VM has no integration by that name (ssh "+p.b.name+" integrations attach <name> <vm>, from your computer)", http.StatusForbidden)
}

// gitPath is what git over smart HTTP asks for: OWNER/REPO(.git)/...
var gitPath = regexp.MustCompile(`^/[^/]+/[^/]+/(info/refs|git-upload-pack|git-receive-pack)$`)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSuffix(strings.ToLower(r.Host), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	name, ok := strings.CutSuffix(host, ".int."+p.b.hv.SSHSuffix)
	if !ok || name == "" || strings.Contains(name, ".") {
		p.forbid(w)
		return
	}
	p.b.mu.RLock()
	v, err := p.b.caller(r.RemoteAddr)
	conn, _ := r.Context().Value(connVMKey{}).(VM)
	var ig Integration
	// same() alone takes a VM made in the same second for the one before.
	if err == nil && conn.same(v) && conn.Name == v.Name {
		ig, err = p.b.store.Attached(v, name)
	} else if err == nil {
		err = errNotFound
	}
	p.b.mu.RUnlock()
	if err != nil {
		if err != errNotFound {
			log.Printf("integration %s for %s: %v", name, r.RemoteAddr, err)
		}
		p.forbid(w)
		return
	}

	var target string
	var header, value string
	path := r.URL.Path
	switch ig.Kind {
	case kindHTTP:
		target, header, value = ig.Target, ig.HeaderName, ig.Secret
	case kindGitHub:
		if rest, ok := strings.CutPrefix(path, "/api"); ok && (rest == "" || rest[0] == '/') {
			if ig.ReadOnly && r.Method != "GET" && r.Method != "HEAD" {
				http.Error(w, "read-only integration: only GET and HEAD", http.StatusForbidden)
				return
			}
			target, header, value = p.githubAPI, "Authorization", "Bearer "+ig.Secret
			r.URL.Path, r.URL.RawPath = rest, strings.TrimPrefix(r.URL.RawPath, "/api")
		} else if gitPath.MatchString(path) {
			if ig.ReadOnly && (strings.HasSuffix(path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack") {
				http.Error(w, "read-only integration: no push", http.StatusForbidden)
				return
			}
			target, header = p.githubURL, "Authorization"
			value = "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+ig.Secret))
		} else {
			http.Error(w, "only git (OWNER/REPO) and the GitHub API (/api/...) go through this integration", http.StatusNotFound)
			return
		}
	default:
		p.forbid(w)
		return
	}
	u, err := url.Parse(target)
	if err != nil {
		p.forbid(w)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport:     p.transport,
		FlushInterval: -1, // streams as they come: server-sent events, git's progress
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			// A webhook is its URL: called as http://<name>.int.<suffix>/.
			if (path == "/" || path == "") && u.Path != "" {
				pr.Out.URL.Path, pr.Out.URL.RawPath = u.Path, u.RawPath
			}
			if header != "" {
				pr.Out.Header.Del(header)
				pr.Out.Header.Set(header, value)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if ig.Kind == kindHTTP {
				hideTarget(res.Header, u, "http://"+name+".int."+p.b.hv.SSHSuffix)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The URL may be the secret: not in the log.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			log.Printf("%s: integration %s of %s: %v", v.Owner, ig.Name, v.Name, err)
			http.Error(w, "the integration's target did not answer", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// hideTarget rewrites the headers of an answer that name the target, as
// a redirect does, to the integration's own address (base): the target's
// path may be the secret, as in a webhook.
func hideTarget(h http.Header, target *url.URL, base string) {
	path := strings.TrimSuffix(target.Path, "/")
	if path == "" {
		return
	}
	for _, k := range []string{"Location", "Content-Location", "Link", "Refresh"} {
		for i, v := range h.Values(k) {
			for _, scheme := range []string{"https://", "http://"} {
				v = strings.ReplaceAll(v, scheme+target.Host+path, base)
			}
			if v = strings.ReplaceAll(v, path, ""); v == "" {
				v = "/"
			}
			h[http.CanonicalHeaderKey(k)][i] = v
		}
	}
}

var errNotPublic = errors.New("not a public address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// public says whether an integration may reach ip: a public address, and
// none of the host's own (local), which may be public too.
func public(ip netip.Addr, local []netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || cgnat.Contains(ip) {
		return false
	}
	for _, l := range local {
		if l.Unmap() == ip {
			return false
		}
	}
	return true
}

// localAddrs are the addresses of the host's interfaces.
func localAddrs() []netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	var list []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			list = append(list, p.Addr())
		}
	}
	return list
}

// publicTransport reaches public addresses only: an integration does not
// take VMs to the host, the LAN or the tailnet. Checked on the address
// dialed, after DNS, so a name cannot resolve its way in.
func publicTransport() *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !public(ap.Addr(), localAddrs()) {
				return errNotPublic
			}
			return nil
		}}
	return &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
}

// dnsClosed says whether the VM at ip may resolve only integrations' names:
// it reaches nothing else. Any address that is no VM gets nothing either.
// Without b.mu: a stale answer for a VM being replaced does no harm, its
// packets go nowhere else anyway.
func (b *Box) dnsClosed(ip netip.Addr) bool {
	idx, ok := vmAt(ip)
	if !ok {
		return true
	}
	v, err := b.store.GetByIndex(idx)
	return err != nil || v.Egress == egressIntegrations
}
