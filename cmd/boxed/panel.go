package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The panel on https://<domain>: each member's VMs, all of them for an
// admin, read only. Pages are server-rendered; panel.js only adds the copy
// buttons. VM pages are on the same site and untrusted, hence the headers in
// panelHeaders and no state change on a GET.

//go:embed panel
var panelFiles embed.FS

var panelPages = func() map[string]*template.Template {
	funcs := template.FuncMap{
		"size": size,
		"pct": func(p float64) string {
			if p <= 0 {
				return "-"
			}
			return fmt.Sprintf("%.0f%%", p)
		},
		"when": func(rfc3339 string) string {
			t, err := time.Parse(time.RFC3339, rfc3339)
			if err != nil {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
	}
	layout := template.Must(template.New("").Funcs(funcs).ParseFS(panelFiles, "panel/layout.html"))
	pages := map[string]*template.Template{}
	for _, name := range []string{"vms", "vm", "me", "message"} {
		pages[name] = template.Must(template.Must(layout.Clone()).ParseFS(panelFiles, "panel/"+name+".html"))
	}
	return pages
}()

var panelStatic = func() http.Handler {
	sub, _ := fs.Sub(panelFiles, "panel")
	return http.FileServerFS(sub)
}()

// panel serves the panel on w's domain with the VMs of this box.
func (b *Box) panel(w *Web) {
	w.memberOf = b.keys.Member
	w.vms = func(owner string) ([]vmJSON, error) {
		_, list, err := b.list(owner, time.Now())
		return list, err
	}
	w.keysOf = b.keys.AuthorizedKeys
	w.sshName = b.name
}

// panelPage is what a page of the panel shows.
type panelPage struct {
	Title   string
	Nav     string // the current section: vms or me
	Email   string // who is logged in; "" on pages without a session
	User    string // their GitHub user; "" if not linked
	Admin   bool
	SSH     string // what members ssh to
	Domain  string
	VMs     []vmJSON
	VM      vmJSON
	Links   []panelLink // of VM
	Keys    []string
	Message string
	Logout  bool      // the message offers to log out
	Action  panelLink // or this
}

type panelLink struct{ Label, URL string }

// panelHeaders keep the browser from framing the panel, sending its forms
// elsewhere, running anything but its own files, or guessing types.
func panelHeaders(h http.Header, static bool) {
	csp := "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	if static {
		csp += "; style-src 'self' 'unsafe-inline'" // the favicon picks its color for dark mode
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	if !static {
		h.Set("Cache-Control", "no-store")
	}
}

func (w *Web) render(rw http.ResponseWriter, status int, name string, p panelPage) {
	p.SSH, p.Domain = w.sshName, w.cfg.Domain
	if p.SSH == "" {
		p.SSH = "boxe"
	}
	panelHeaders(rw.Header(), false)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	if err := panelPages[name].ExecuteTemplate(rw, "layout", p); err != nil {
		log.Printf("web: panel %s: %v", name, err)
	}
}

// message is a page with only a sentence (and a log out button).
func (w *Web) message(rw http.ResponseWriter, status int, title, msg string) {
	w.render(rw, status, "message", panelPage{Title: title, Message: msg})
}

// servePanel answers the requests to <domain> that are not the login, the
// logout or /healthz.
func (w *Web) servePanel(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/favicon.ico" {
		r.URL.Path = "/static/favicon-32.png" // asked for by browsers, also mid-login
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		panelHeaders(rw.Header(), true)
		rw.Header().Set("Cache-Control", "public, max-age=3600")
		panelStatic.ServeHTTP(rw, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rw.Header().Set("Allow", "GET, HEAD")
		w.message(rw, http.StatusMethodNotAllowed, "Not allowed", "This page can only be read.")
		return
	}
	s, ok := w.session(r, w.cfg.Domain)
	if !ok {
		http.Redirect(rw, r, "/__boxe/login?"+url.Values{"r": {"https://" + w.cfg.Domain + r.URL.RequestURI()}}.Encode(), http.StatusFound)
		return
	}
	p := panelPage{Email: s.E}
	p.User, p.Admin = w.memberOf(s.E)
	switch path := r.URL.Path; {
	case path == "/":
		w.vmList(rw, p)
	case path == "/me":
		p.Title, p.Nav = "Account", "me"
		if p.User != "" {
			p.Keys = strings.Split(strings.TrimSpace(w.keysOf(p.User)), "\n")
		}
		w.render(rw, http.StatusOK, "me", p)
	case strings.HasPrefix(path, "/vm/"):
		w.vmDetail(rw, p, strings.TrimPrefix(path, "/vm/"))
	default:
		w.notFound(rw, p)
	}
}

func (w *Web) notFound(rw http.ResponseWriter, p panelPage) {
	p.Title, p.Message = "Not found", "There is nothing here."
	w.render(rw, http.StatusNotFound, "message", p)
}

// visible is what p's member may see: their VMs, every VM for an admin,
// none if their email is linked to nobody.
func (w *Web) visible(p panelPage) ([]vmJSON, error) {
	switch {
	case p.User == "":
		return nil, nil
	case p.Admin:
		return w.vms("")
	}
	return w.vms(p.User)
}

func (w *Web) vmList(rw http.ResponseWriter, p panelPage) {
	p.Title, p.Nav = "VMs", "vms"
	vms, err := w.visible(p)
	if err != nil {
		log.Printf("web: panel: %v", err)
		w.message(rw, http.StatusInternalServerError, "Error", "The VMs could not be listed, try again.")
		return
	}
	// An admin's own VMs first, then everyone else's, each by name.
	sort.SliceStable(vms, func(i, j int) bool { return vms[i].Owner == p.User && vms[j].Owner != p.User })
	p.VMs = vms
	w.render(rw, http.StatusOK, "vms", p)
}

func (w *Web) vmDetail(rw http.ResponseWriter, p panelPage, name string) {
	vms, err := w.visible(p)
	if err != nil {
		log.Printf("web: panel: %v", err)
		w.message(rw, http.StatusInternalServerError, "Error", "The VM could not be read, try again.")
		return
	}
	for _, v := range vms {
		if v.Name != name {
			continue
		}
		p.Title, p.Nav, p.VM = v.Name, "vms", v
		host := func(label string) string { return "https://" + label + "." + w.cfg.Domain }
		p.Links = []panelLink{{"Web", host(v.Name)}}
		if v.Owner == p.User {
			p.Links = append(p.Links, panelLink{"Agent", host(v.Name + "--agent")}, panelLink{"Desktop", host(v.Name + "--desktop")})
		}
		w.render(rw, http.StatusOK, "vm", p)
		return
	}
	w.notFound(rw, p)
}
