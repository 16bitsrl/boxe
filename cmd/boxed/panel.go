package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
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
// admin, which they can restart and delete. Pages are server-rendered;
// panel.js only adds the copy buttons. VM pages are on the same site and
// untrusted, hence the headers in panelHeaders, no state change on a GET, and
// the origin check and the session's token on every form (act).

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
		// static is the address of a file in panel/static, which changes
		// with the file: browsers keep them for an hour.
		"static": func(name string) string {
			b, err := panelFiles.ReadFile("panel/static/" + name)
			if err != nil {
				panic(err)
			}
			h := sha256.Sum256(b)
			return fmt.Sprintf("/static/%s?v=%x", name, h[:5])
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
	w.restart = func(user string, admin bool, name string) error {
		_, err := b.restart(user, admin, name)
		return err
	}
	w.remove = func(user string, admin bool, name string) error {
		_, err := b.remove(user, admin, name)
		return err
	}
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
	Manage  bool        // they may restart and delete VM
	CSRF    string      // the token of the session, for forms
	Error   string      // what went wrong with the last form
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
	if name, action, ok := panelAction(r.URL.Path); ok && r.Method == http.MethodPost {
		w.act(rw, r, name, action)
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
	p := w.page(s)
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

// page is a page of the panel for the session s.
func (w *Web) page(s claims) panelPage {
	p := panelPage{Email: s.E, CSRF: w.csrfToken(s)}
	p.User, p.Admin = w.memberOf(s.E)
	return p
}

// csrfToken is what the panel's forms carry: bound to the session (the
// email and the login at the IdP), unknown to VM pages, which can send
// forms here with the member's cookie but cannot read the panel.
func (w *Web) csrfToken(s claims) string {
	return base64.RawURLEncoding.EncodeToString(w.mac(fmt.Sprintf("csrf\x00%s\x00%d", s.E, s.A)))
}

// find is the VM name among those p's member may see.
func (w *Web) find(rw http.ResponseWriter, p panelPage, name string) (vmJSON, bool) {
	vms, err := w.visible(p)
	if err != nil {
		log.Printf("web: panel: %v", err)
		w.message(rw, http.StatusInternalServerError, "Error", "The VM could not be read, try again.")
		return vmJSON{}, false
	}
	for _, v := range vms {
		if v.Name == name {
			return v, true
		}
	}
	w.notFound(rw, p)
	return vmJSON{}, false
}

func (w *Web) vmDetail(rw http.ResponseWriter, p panelPage, name string) {
	if v, ok := w.find(rw, p, name); ok {
		w.vmPage(rw, http.StatusOK, p, v)
	}
}

func (w *Web) vmPage(rw http.ResponseWriter, status int, p panelPage, v vmJSON) {
	p.Title, p.Nav, p.VM = v.Name, "vms", v
	host := func(label string) string { return "https://" + label + "." + w.cfg.Domain }
	p.Links = []panelLink{{"Web", host(v.Name)}}
	if v.Owner == p.User {
		p.Links = append(p.Links, panelLink{"Agent", host(v.Name + "--agent")}, panelLink{"Desktop", host(v.Name + "--desktop")})
	}
	p.Manage = v.Owner == p.User || p.Admin
	w.render(rw, status, "vm", p)
}

// panelAction splits /vm/<name>/<action>, for the actions of the panel.
func panelAction(path string) (name, action string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/vm/")
	name, action, _ = strings.Cut(rest, "/")
	return name, action, ok && (action == "restart" || action == "delete")
}

// act runs a form of the panel: only from the panel itself, in the session
// it was made for, on a VM the member may manage.
func (w *Web) act(rw http.ResponseWriter, r *http.Request, name, action string) {
	s, ok := w.session(r, w.cfg.Domain)
	var p panelPage
	if ok {
		p = w.page(s)
	}
	if err := http.NewCrossOriginProtection().Check(r); err != nil {
		p.Title, p.Message = "Not allowed", "VMs can only be changed from this panel."
		w.render(rw, http.StatusForbidden, "message", p)
		return
	}
	if !ok || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(p.CSRF)) {
		p.Title, p.Message = "Page expired", "Open the page again and retry."
		w.render(rw, http.StatusForbidden, "message", p)
		return
	}
	// Whoever sees a VM may manage it; restart and remove check it again.
	v, ok := w.find(rw, p, name)
	if !ok {
		return
	}
	if action == "delete" && r.PostFormValue("confirm") != v.Name {
		p.Error = "Type " + v.Name + " to delete it."
		w.vmPage(rw, http.StatusBadRequest, p, v)
		return
	}
	do := map[string]func(string, bool, string) error{"restart": w.restart, "delete": w.remove}[action]
	if err := do(p.User, p.Admin, v.Name); err != nil {
		p.Error = err.Error() + "."
		w.vmPage(rw, http.StatusInternalServerError, p, v)
		return
	}
	log.Printf("web: %s (%s): %s %s", s.E, p.User, action, v.Name)
	if action == "delete" {
		p.Title, p.Message = "Deleted", v.Name+" is deleted."
		w.render(rw, http.StatusOK, "message", p)
		return
	}
	http.Redirect(rw, r, "/vm/"+v.Name, http.StatusSeeOther)
}
