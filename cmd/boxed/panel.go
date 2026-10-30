package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The panel on https://<domain>: each member's VMs, all of them for an
// admin, which they can restart and delete; members make new VMs and copy
// theirs. Every action shows the command that does it over SSH, and runs
// exactly that command: the panel can do nothing ssh boxe cannot.
// Pages are server-rendered; panel.js only adds the copy buttons and keeps
// the commands of forms up to date. VM pages are on the same site and
// untrusted, hence the headers in panelHeaders, no state change on a GET, and
// the origin check and the session's token on every form (act).

//go:embed panel
var panelFiles embed.FS

var panelPages = func() map[string]*template.Template {
	funcs := template.FuncMap{
		"size": size,
		"list": func(s ...string) []string { return s },
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
	for _, name := range []string{"vms", "vm", "console", "new", "me", "host", "message"} {
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
	w.run = func(user, line string) ([]byte, error) {
		var out bytes.Buffer
		err := b.run(&out, user, line, "")
		return out.Bytes(), err
	}
	w.host = b.hostInfo
	w.samples = b.store.Samples
	w.images = b.hv.Images
	w.console = func(name string) (string, error) {
		v, err := b.store.Get(name)
		if err != nil {
			return "", err
		}
		return b.hv.Console(v, consoleLines)
	}
	w.sshName = b.name
}

// panelPage is what a page of the panel shows.
type panelPage struct {
	Title   string
	Nav     string // the current section: vms, host or me
	Email   string // who is logged in; "" on pages without a session
	User    string // their GitHub user; "" if not linked
	Admin   bool
	SSH     string // what members ssh to
	Domain  string
	VMs     []vmJSON
	VM      vmJSON
	Links   []panelLink       // of VM
	Manage  bool              // they may restart and delete VM
	CSRF    string            // the token of the session, for forms
	Error   string            // what went wrong with the last form
	Host    hostInfo          // for an admin
	Usage   usage             // of VM
	Owns    bool              // VM is theirs
	Form    newVM             // as typed in the form of a new VM
	Default newVM             // what a new VM is without options
	Images  []image           // to make it from
	Cmds    map[string]string // the command of each action, after ssh boxe
	Console string            // what VM printed lately
	Keys    []string
	Message string
	Logout  bool      // the message offers to log out
	Action  panelLink // or this
}

type panelLink struct{ Label, URL string }

// newVM is the form of a new VM; a copy has only the name.
type newVM struct{ Name, Image, CPU, Mem string }

// defaultVM is a new VM without options, as parseVMFlags makes it.
var defaultVM = newVM{CPU: "2", Mem: "2G"}

// newCommand is the command that makes n; defaultImage is the image new
// VMs start from without --image. Options at their default are left out.
func newCommand(n newVM, defaultImage string) []string {
	words := []string{"new"}
	if n.Name != "" {
		words = append(words, n.Name)
	}
	for _, o := range []struct{ flag, value, def string }{
		{"--image", n.Image, defaultImage}, {"--cpu", n.CPU, defaultVM.CPU}, {"--mem", n.Mem, defaultVM.Mem}} {
		if o.value != "" && o.value != o.def {
			words = append(words, o.flag, o.value)
		}
	}
	return words
}

// copyCommand is the command that copies the VM src, to name if given.
func copyCommand(src, name string) []string {
	if name == "" {
		return []string{"cp", src}
	}
	return []string{"cp", src, name}
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9._/:=@%+-]+$`)

// cmdLine writes words as a command line, quoted as a shell wants them
// (panel.js does the same).
func cmdLine(words []string) string {
	var q []string
	for _, w := range words {
		if !safeWord.MatchString(w) {
			w = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
		q = append(q, w)
	}
	return strings.Join(q, " ")
}

// oneWord refuses what would not stay one word of the command line, even
// quoted: run reads --agent before it reads quotes.
func oneWord(field, value string) error {
	if strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return nil
}

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
	case path == "/new":
		w.newPage(rw, http.StatusOK, p, defaultVM)
	case path == "/me":
		p.Title, p.Nav = "Account", "me"
		if p.User != "" {
			p.Keys = strings.Split(strings.TrimSpace(w.keysOf(p.User)), "\n")
		}
		w.render(rw, http.StatusOK, "me", p)
	case path == "/host" && p.Admin:
		p.Title, p.Nav, p.Host = "Host", "host", w.host()
		w.render(rw, http.StatusOK, "host", p)
	case strings.HasPrefix(path, "/vm/"):
		switch name, sub, _ := strings.Cut(strings.TrimPrefix(path, "/vm/"), "/"); sub {
		case "":
			w.vmDetail(rw, p, name, r.URL.Query().Get("range"))
		case "console":
			w.consolePage(rw, p, name)
		default:
			w.notFound(rw, p)
		}
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

func (w *Web) vmDetail(rw http.ResponseWriter, p panelPage, name, rng string) {
	if v, ok := w.find(rw, p, name); ok {
		w.vmPage(rw, http.StatusOK, p, v, rng)
	}
}

// vmPage is the page of v, with its usage over rng (see usageOf).
func (w *Web) vmPage(rw http.ResponseWriter, status int, p panelPage, v vmJSON, rng string) {
	p.Title, p.Nav, p.VM = v.Name, "vms", v
	if _, ok := usageRanges[rng]; !ok {
		rng = "24h"
	}
	now := time.Now()
	samples, err := w.samples(v.Name, now.Add(-usageRanges[rng]))
	if err != nil {
		log.Printf("web: panel: samples of %s: %v", v.Name, err)
	}
	p.Usage = usageOf(v, samples, rng, now)
	host := func(label string) string { return "https://" + label + "." + w.cfg.Domain }
	p.Links = []panelLink{{"Web", host(v.Name)}}
	p.Owns = v.Owner == p.User
	if p.Owns {
		p.Links = append(p.Links, panelLink{"Agent", host(v.Name + "--agent")}, panelLink{"Desktop", host(v.Name + "--desktop")})
	}
	p.Manage = v.Owner == p.User || p.Admin
	p.Cmds = map[string]string{"cp": cmdLine(copyCommand(v.Name, p.Form.Name)),
		"restart": cmdLine([]string{"restart", v.Name}), "rm": cmdLine([]string{"rm", v.Name})}
	w.render(rw, status, "vm", p)
}

// consoleLines is how much of a VM's console the panel shows.
const consoleLines = 200

// consolePage shows what the VM name printed lately, to whoever may manage
// it: what they see is what they manage (see visible).
func (w *Web) consolePage(rw http.ResponseWriter, p panelPage, name string) {
	v, ok := w.find(rw, p, name)
	if !ok {
		return
	}
	p.Title, p.Nav, p.VM = v.Name+" console", "vms", v
	var err error
	if p.Console, err = w.console(v.Name); err != nil {
		log.Printf("web: panel: console of %s: %v", v.Name, err)
		p.Error = "The console could not be read, try again."
	}
	w.render(rw, http.StatusOK, "console", p)
}

// newPage is the form of a new VM, filled with n.
func (w *Web) newPage(rw http.ResponseWriter, status int, p panelPage, n newVM) {
	p.Title, p.Nav, p.Form, p.Default = "New VM", "vms", n, defaultVM
	if p.User != "" {
		p.Images, p.Default.Image = w.imagesAndDefault()
	}
	p.Cmds = map[string]string{"new": cmdLine(newCommand(n, p.Default.Image))}
	w.render(rw, status, "new", p)
}

// imagesAndDefault are the images new VMs can start from and the name of
// the one they start from without --image.
func (w *Web) imagesAndDefault() ([]image, string) {
	images, err := w.images()
	if err != nil {
		log.Printf("web: panel: images: %v", err) // the form goes on with the default image
	}
	for _, im := range images {
		if im.Default {
			return images, im.Name
		}
	}
	return images, ""
}

// panelAction splits /vm/<name>/<action> and /new, the forms of the panel.
func panelAction(path string) (name, action string, ok bool) {
	if path == "/new" {
		return "", "new", true
	}
	rest, ok := strings.CutPrefix(path, "/vm/")
	name, action, _ = strings.Cut(rest, "/")
	return name, action, ok && (action == "restart" || action == "delete" || action == "copy")
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
	if action == "new" {
		n := newVM{Name: strings.TrimSpace(r.PostFormValue("name")), Image: r.PostFormValue("image"),
			CPU: r.PostFormValue("cpu"), Mem: r.PostFormValue("mem")}
		_, def := w.imagesAndDefault()
		w.createVM(rw, r, p, s.E, n, nil, newCommand(n, def))
		return
	}
	// Whoever sees a VM may manage it; restart and remove check it again,
	// and copy only copies the member's own.
	v, ok := w.find(rw, p, name)
	if !ok {
		return
	}
	if action == "copy" {
		n := newVM{Name: strings.TrimSpace(r.PostFormValue("name"))}
		w.createVM(rw, r, p, s.E, n, &v, copyCommand(v.Name, n.Name))
		return
	}
	if action == "delete" && r.PostFormValue("confirm") != v.Name {
		p.Error = "Type " + v.Name + " to delete it."
		w.vmPage(rw, http.StatusBadRequest, p, v, "")
		return
	}
	cmd := map[string]string{"restart": "restart", "delete": "rm"}[action]
	if _, err := w.run(p.User, cmdLine([]string{cmd, v.Name})+" --json"); err != nil {
		p.Error = sentence(err)
		w.vmPage(rw, http.StatusInternalServerError, p, v, "")
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

// createVM makes n for p's member, or a copy of src, with the command
// words, and goes to it; mistakes go back to the form.
func (w *Web) createVM(rw http.ResponseWriter, r *http.Request, p panelPage, email string, n newVM, src *vmJSON, words []string) {
	if p.User == "" {
		p.Title, p.Message = "Not allowed", "Ask an admin to add you to the team."
		w.render(rw, http.StatusForbidden, "message", p)
		return
	}
	did := "new"
	if src != nil {
		did = "copy " + src.Name + " to"
	}
	var j vmJSON
	err := errors.Join(oneWord("name", n.Name), oneWord("image", n.Image), oneWord("CPU", n.CPU), oneWord("RAM", n.Mem))
	if err == nil {
		var out []byte
		out, err = w.run(p.User, cmdLine(words)+" --json")
		json.Unmarshal(out, &j)
	}
	if err != nil {
		p.Error = sentence(err)
		p.Form = n
		if src != nil {
			w.vmPage(rw, http.StatusBadRequest, p, *src, "")
		} else {
			w.newPage(rw, http.StatusBadRequest, p, n)
		}
		return
	}
	log.Printf("web: %s (%s): %s %s", email, p.User, did, j.Name)
	http.Redirect(rw, r, "/vm/"+j.Name, http.StatusSeeOther)
}

// sentence is err for the panel: as ssh boxe says it, as a sentence.
func sentence(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}
