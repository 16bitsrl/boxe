package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// panelTest is the panel on https://boxe.example with real VMs (on a fake
// host): alice has test and nap, bob has bobs-vm, mattia, the admin, mine.
type panelTest struct {
	*webTest
	c *cli
}

func newPanelTest(t *testing.T) *panelTest {
	c := newCLI(t, "alice", "bob", "mattia")
	c.box.hv.WebDomain = "boxe.example"
	c.must("alice", "new test")
	c.must("alice", "new nap")
	c.must("bob", "new bobs-vm")
	c.must("mattia", "new mine")
	c.box.keys.mu.Lock()
	c.box.keys.emails = map[string]string{"alice": "alice@example.com", "bob": "bob@example.com", "mattia": "mattia@example.com"}
	c.box.keys.admins = map[string]bool{"mattia": true}
	c.box.keys.byUser = map[string]string{"alice": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlice alice@laptop\n"}
	c.box.keys.mu.Unlock()
	wt := &webTest{t: t, idp: newFakeIdP(t)}
	wt.web = NewWeb(WebConfig{
		Domain:       "boxe.example",
		Issuer:       wt.idp.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		OrgID:        testOrg,
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
	}, c.box.store)
	c.box.panel(wt.web)
	return &panelTest{webTest: wt, c: c}
}

// page logs in as code (if needed) and returns the page at path of the panel.
func (b *browser) page(path string) (int, string) {
	res := b.get("https://boxe.example"+path, nil)
	return res.StatusCode, body(res)
}

// fromPanel are the headers a browser sends with a form of the panel.
var fromPanel = http.Header{"Sec-Fetch-Site": {"same-origin"}, "Origin": {"https://boxe.example"}}

// post sends form to path of the panel, with header.
func (b *browser) post(path string, form url.Values, header http.Header) *http.Response {
	h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	for k, v := range header {
		h[k] = v
	}
	return b.sendBody("POST", "https://boxe.example"+path, h, form.Encode())
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf is the token in the forms of page.
func csrf(t *testing.T, page string) string {
	t.Helper()
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no csrf field in:\n%s", page)
	}
	return m[1]
}

// Opening the panel logs the member in at the IdP, then shows their VMs,
// and only theirs.
func TestPanelShowsMyVMs(t *testing.T) {
	pt := newPanelTest(t)
	code, page := pt.browser("member").page("/")
	if code != 200 || pt.idp.authorizes.Load() != 1 {
		t.Fatalf("%d, %d logins:\n%s", code, pt.idp.authorizes.Load(), page)
	}
	for _, want := range []string{">test<", ">nap<", "https://test.boxe.example", "ssh test.boxe", "running"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	for _, other := range []string{"bobs-vm", "mine"} {
		if strings.Contains(page, other) {
			t.Errorf("alice sees %s", other)
		}
	}
}

// After the login the member lands on the page they asked for.
func TestPanelLoginReturnsToThePage(t *testing.T) {
	pt := newPanelTest(t)
	res := pt.browser("member").get("https://boxe.example/vm/test", nil)
	if page := body(res); res.StatusCode != 200 || !strings.Contains(page, "ssh test.boxe") {
		t.Fatalf("%d:\n%s", res.StatusCode, page)
	}
	b := pt.browser("member")
	b.page("/")
	// Logged in, the login goes straight back: only to a page of the panel.
	res = b.do("https://boxe.example/__boxe/login?"+url.Values{"r": {"https://boxe.example//evil.example/x"}}.Encode(), nil)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		t.Errorf("//evil.example: %d %q", res.StatusCode, loc)
	}
	for _, bad := range []string{"https://boxe.example.evil/", "http://boxe.example/", "https://user@boxe.example/", "https://boxe.example:8443/"} {
		res := b.do("https://boxe.example/__boxe/login?"+url.Values{"r": {bad}}.Encode(), nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("r=%s: %d", bad, res.StatusCode)
		}
	}
}

// The detail of a VM: what it is, what it uses and how to reach it.
func TestPanelVMDetail(t *testing.T) {
	pt := newPanelTest(t)
	h := pt.c.host
	h.cpu[unit("test")] = 1e9
	pt.c.box.sleepIdle(time.Now().Add(-10 * time.Second))
	h.cpu[unit("test")] = 6e9 // 5 s of CPU in 10 s
	h.mem = map[string]int64{unit("test"): 700 << 20}
	h.disk = "boxe/vms/test\t5476083302\n"
	code, page := pt.browser("member").page("/vm/test")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{"ssh test.boxe", "https://test.boxe.example", "https://test--agent.boxe.example",
		"https://test--desktop.boxe.example", "https://test--3000.boxe.example", "50%", "700M", "5.1G", "10.42.0.2", "running"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	if code, page := pt.browser("member").page("/"); code != 200 || !strings.Contains(page, "5.1G") || !strings.Contains(page, "50%") {
		t.Errorf("the list lacks the usage: %d\n%s", code, page)
	}
}

// Someone else's VM does not exist for a member, as in ssh boxe.
func TestPanelHidesOthersVMs(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	for _, path := range []string{"/vm/bobs-vm", "/vm/nope", "/vm/", "/nope"} {
		if code, page := b.page(path); code != 404 || strings.Contains(page, "bob") {
			t.Errorf("%s: %d\n%s", path, code, page)
		}
	}
}

// The admin sees every VM, theirs first, with its owner, and the detail of
// any; the agent and the desktop stay the owner's.
func TestPanelAdminSeesAll(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("admin")
	code, page := b.page("/")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	mine, bobs, test := strings.Index(page, ">mine<"), strings.Index(page, ">bobs-vm<"), strings.Index(page, ">test<")
	if mine < 0 || bobs < 0 || test < 0 || mine > bobs || bobs > test || !strings.Contains(page, "alice") {
		t.Errorf("order mine %d, bobs-vm %d, test %d:\n%s", mine, bobs, test, page)
	}
	code, page = b.page("/vm/bobs-vm")
	if code != 200 || !strings.Contains(page, "bob") || !strings.Contains(page, "ssh bobs-vm.boxe") {
		t.Fatalf("%d:\n%s", code, page)
	}
	if strings.Contains(page, "bobs-vm--agent") || strings.Contains(page, "bobs-vm--desktop") {
		t.Errorf("links to bob's agent or desktop for the admin:\n%s", page)
	}
}

// A member of the IdP not in the users file sees no VM and is told why.
func TestPanelUnlinkedMember(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("stranger")
	code, page := b.page("/")
	if code != 200 || !strings.Contains(page, "stranger@example.com") || !strings.Contains(page, "admin") {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, vm := range []string{"test", "bobs-vm", "mine"} {
		if strings.Contains(page, ">"+vm+"<") {
			t.Errorf("sees %s", vm)
		}
	}
	if code, _ := b.page("/vm/test"); code != 404 {
		t.Errorf("detail of someone's VM: %d", code)
	}
}

// Who I am for boxe, my keys, how to set up SSH, and logging out.
func TestPanelMe(t *testing.T) {
	pt := newPanelTest(t)
	code, page := pt.browser("member").page("/me")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{"alice@example.com", "alice", "AAAAC3NzaC1lZDI1NTE5AAAAIAlice", "ssh boxe ssh-config",
		"ssh boxe known-hosts", `method="post"`, `action="/logout"`} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
}

// A session on a VM host is no session on the panel: VMs are untrusted.
func TestPanelIgnoresVMSessions(t *testing.T) {
	pt := newPanelTest(t)
	c := pt.web.sessionCookie("alice@example.com", "test.boxe.example", time.Now().UnixMilli())
	b := pt.browser("member")
	res := b.do("https://boxe.example/", http.Header{"Cookie": {c.Name + "=" + c.Value}})
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "/__boxe/login") {
		t.Errorf("%d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// Every page of the panel locks down what the browser may do with it, and
// carries no inline script or style, which the policy forbids.
func TestPanelSecurityHeaders(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	b.page("/")
	for _, path := range []string{"/", "/vm/test", "/me", "/nope", "/static/panel.css"} {
		res := b.do("https://boxe.example"+path, nil)
		h := res.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'self'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: %v", path, h)
		}
		if strings.HasPrefix(path, "/static/") {
			continue
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, h.Get("Cache-Control"))
		}
		page := body(res)
		if strings.Contains(page, "<script>") || strings.Contains(page, "style=") || strings.Contains(page, "<style") || strings.Contains(page, "onclick=") {
			t.Errorf("%s: inline script or style:\n%s", path, page)
		}
	}
	if res := b.do("https://boxe.example/healthz", nil); res.StatusCode != 200 || body(res) != "ok\n" {
		t.Errorf("healthz: %d", res.StatusCode)
	}
}

// The browser asks for /favicon.ico on its own, also during a login: it
// gets the icon, and no new login that would replace the one going on.
func TestPanelFaviconNeedsNoLogin(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	res := b.do("https://boxe.example/favicon.ico", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || len(res.Cookies()) != 0 {
		t.Errorf("%d %q, cookies %v", res.StatusCode, res.Header.Get("Content-Type"), res.Cookies())
	}
}

// exists says whether the VM name is still there.
func (pt *panelTest) exists(name string) bool {
	_, err := pt.c.box.store.Get(name)
	return err == nil
}

// forget clears the commands the host ran so far.
func (pt *panelTest) forget() {
	pt.c.host.mu.Lock()
	pt.c.host.cmds = nil
	pt.c.host.mu.Unlock()
}

// The owner restarts their VM from its page, and deletes it once they type
// its name.
func TestPanelRestartAndDelete(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	_, page := b.page("/vm/test")
	token := csrf(t, page)
	for _, want := range []string{`action="/vm/test/restart"`, `action="/vm/test/delete"`, `name="confirm"`} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}

	pt.forget()
	res := b.post("/vm/test/restart", url.Values{"csrf": {token}}, fromPanel)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/vm/test" {
		t.Errorf("restart: %d %q\n%s", res.StatusCode, res.Header.Get("Location"), body(res))
	}
	if !pt.c.host.ran("systemctl stop boxe-vm-test") || !pt.c.host.ran("systemd-run") {
		t.Errorf("not restarted: %v", pt.c.host.cmds)
	}

	for _, typed := range []string{"", "tes", "nap"} {
		res := b.post("/vm/test/delete", url.Values{"csrf": {token}, "confirm": {typed}}, fromPanel)
		if page := body(res); res.StatusCode != http.StatusBadRequest || !strings.Contains(page, "Type test") || !pt.exists("test") {
			t.Errorf("confirm %q: %d\n%s", typed, res.StatusCode, page)
		}
	}
	res = b.post("/vm/test/delete", url.Values{"csrf": {token}, "confirm": {"test"}}, fromPanel)
	if page := body(res); res.StatusCode != 200 || !strings.Contains(page, "test is deleted") || pt.exists("test") {
		t.Errorf("delete: %d\n%s", res.StatusCode, page)
	}
	if code, _ := b.page("/vm/test"); code != 404 {
		t.Errorf("deleted VM's page: %d", code)
	}
}

// A VM's page is on the same site as the panel, so its forms reach the panel
// with the member's cookie: they must do nothing. Neither does a form without
// the token of this very session, nor a GET.
func TestPanelActionsOnlyFromThePanel(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	_, page := b.page("/vm/test")
	token := csrf(t, page)
	time.Sleep(2 * time.Millisecond)
	other := pt.browser("member") // alice again, logged in later
	_, otherPage := other.page("/vm/test")
	_, bobPage := pt.browser("bob").page("/vm/bobs-vm")
	if csrf(t, otherPage) == token || csrf(t, bobPage) == token {
		t.Fatal("two sessions share a token")
	}

	fromVM := http.Header{"Sec-Fetch-Site": {"same-site"}, "Origin": {"https://test.boxe.example"}}
	oldBrowser := http.Header{"Origin": {"https://test.boxe.example"}} // no Sec-Fetch-Site
	pt.forget()
	for _, c := range []struct {
		name   string
		b      *browser
		form   url.Values
		header http.Header
	}{
		{"from a VM's page", b, url.Values{"csrf": {token}, "confirm": {"test"}}, fromVM},
		{"from a VM's page, old browser", b, url.Values{"csrf": {token}, "confirm": {"test"}}, oldBrowser},
		{"no token", b, url.Values{"confirm": {"test"}}, fromPanel},
		{"forged token", b, url.Values{"csrf": {"x" + token}, "confirm": {"test"}}, fromPanel},
		{"token of another session", other, url.Values{"csrf": {token}, "confirm": {"test"}}, fromPanel},
		{"token of bob", b, url.Values{"csrf": {csrf(t, bobPage)}, "confirm": {"test"}}, fromPanel},
		{"no session", pt.browser("member"), url.Values{"csrf": {token}, "confirm": {"test"}}, fromPanel},
	} {
		for _, action := range []string{"restart", "delete"} {
			res := c.b.post("/vm/test/"+action, c.form, c.header)
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s, %s: %d", c.name, action, res.StatusCode)
			}
		}
	}
	// The member, still logged in, is not asked to log in again.
	if page := body(b.post("/vm/test/delete", url.Values{"csrf": {token}}, fromVM)); strings.Contains(page, "Log in") || !strings.Contains(page, `href="/"`) {
		t.Errorf("refused page:\n%s", page)
	}
	res := b.do("https://boxe.example/vm/test/delete?"+url.Values{"csrf": {token}, "confirm": {"test"}}.Encode(), fromPanel)
	if res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotFound {
		t.Errorf("GET delete: %d", res.StatusCode)
	}
	if !pt.exists("test") || pt.c.host.ran("systemctl stop") {
		t.Errorf("the VM was touched: %v", pt.c.host.cmds)
	}
}

// A member cannot touch someone else's VM, as over SSH; the admin can.
func TestPanelActionsOnOthersVMs(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	_, page := b.page("/vm/test")
	token := csrf(t, page)
	pt.forget()
	for _, action := range []string{"restart", "delete"} {
		res := b.post("/vm/bobs-vm/"+action, url.Values{"csrf": {token}, "confirm": {"bobs-vm"}}, fromPanel)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s bobs-vm: %d", action, res.StatusCode)
		}
	}
	if !pt.exists("bobs-vm") || pt.c.host.ran("systemctl stop") {
		t.Errorf("alice touched bobs-vm: %v", pt.c.host.cmds)
	}

	admin := pt.browser("admin")
	_, page = admin.page("/vm/bobs-vm")
	token = csrf(t, page)
	if res := admin.post("/vm/bobs-vm/restart", url.Values{"csrf": {token}}, fromPanel); res.StatusCode != http.StatusSeeOther {
		t.Errorf("admin restart: %d", res.StatusCode)
	}
	res := admin.post("/vm/bobs-vm/delete", url.Values{"csrf": {token}, "confirm": {"bobs-vm"}}, fromPanel)
	if res.StatusCode != 200 || pt.exists("bobs-vm") {
		t.Errorf("admin delete: %d", res.StatusCode)
	}
}

// The style and the script are cached by browsers, so their address changes
// with them: a new boxed is never shown with the old ones.
func TestPanelStaticFilesChangeAddress(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	_, page := b.page("/")
	for _, file := range []string{"panel.css", "panel.js"} {
		m := regexp.MustCompile(`"(/static/` + regexp.QuoteMeta(file) + `\?v=[0-9a-f]{8,})"`).FindStringSubmatch(page)
		if m == nil {
			t.Errorf("no versioned %s in:\n%s", file, page)
			continue
		}
		if res := b.do("https://boxe.example"+m[1], nil); res.StatusCode != 200 {
			t.Errorf("%s: %d", m[1], res.StatusCode)
		}
	}
}

// The admin's page about the host: what is wrong, the team and the backups.
// Members do not see it.
func TestPanelHost(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.c.box
	dir := t.TempDir()
	b.backupConf, b.backupLast, b.backupStatus = filepath.Join(dir, "backup.conf"), filepath.Join(dir, "backup-last"), filepath.Join(dir, "backup-status.json")
	os.WriteFile(b.backupConf, nil, 0o600)
	start := time.Now().Add(-3 * time.Hour)
	os.WriteFile(b.backupLast, []byte(fmt.Sprint(start.Add(-40*time.Hour).Unix())), 0o644)
	os.WriteFile(b.backupStatus, []byte(fmt.Sprintf(`{"start":%d,"end":%d,"failed":["nap"]}`, start.Unix(), start.Add(25*time.Minute).Unix())), 0o644)
	b.keys.mu.Lock()
	b.keys.users = []string{"alice", "bob", "mattia", "carol"}
	b.keys.mu.Unlock()

	member := pt.browser("member")
	if code, page := member.page("/host"); code != 404 || strings.Contains(page, `href="/host"`) {
		t.Errorf("a member: %d\n%s", code, page)
	}
	code, page := pt.browser("admin").page("/host")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{
		`href="/host" aria-current="page"`,
		"backup older than 36h", // what /healthz says
		"alice@example.com", "carol", "admin",
		"25m", ">nap<", start.Local().Format("2006-01-02 15:04"),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}

	// With every VM asleep, no dash for the RAM they take.
	for _, vm := range []string{"alice test", "alice nap", "bob bobs-vm", "mattia mine"} {
		user, name, _ := strings.Cut(vm, " ")
		pt.c.must(user, "sleep "+name)
	}
	if _, page := pt.browser("admin").page("/host"); !strings.Contains(page, "<dd>none</dd>") {
		t.Errorf("no VM awake:\n%s", page)
	}
}

// The page of a VM charts what it used over the last 24 hours, 7 or 30 days,
// from the samples of every minute.
func TestPanelUsageCharts(t *testing.T) {
	pt := newPanelTest(t)
	now := time.Now()
	for _, s := range []Sample{
		{At: now.Add(-3 * 24 * time.Hour), CPU: 190, Mem: 900 << 20, Disk: 6 << 30},
		{At: now.Add(-2 * time.Hour), CPU: 40, Mem: 600 << 20, Disk: 5 << 30},
		{At: now.Add(-time.Hour), CPU: 80, Mem: 700 << 20, Disk: 5 << 30},
	} {
		if err := pt.c.box.store.AddSample("test", s); err != nil {
			t.Fatal(err)
		}
	}
	member := pt.browser("member")
	code, page := member.page("/vm/test")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{`href="/vm/test?range=24h" aria-current="page"`, `href="/vm/test?range=7d"`, `href="/vm/test?range=30d"`,
		"<h3>CPU</h3>", "<h3>RAM</h3>", "<h3>Disk</h3>", `class="line"`, "peak 80%", "peak 700M", "200%",
		`class="dots" d="M`} { // two samples an hour apart: two lone points
		if !strings.Contains(page, want) {
			t.Errorf("24h: no %q in:\n%s", want, page)
		}
	}
	if strings.Contains(page, "190%") {
		t.Errorf("24h shows a sample of 3 days ago")
	}
	_, page = member.page("/vm/test?range=7d")
	for _, want := range []string{`href="/vm/test?range=7d" aria-current="page"`, "peak 190%", "peak 900M"} {
		if !strings.Contains(page, want) {
			t.Errorf("7d: no %q in:\n%s", want, page)
		}
	}
	if _, page := member.page("/vm/test?range=1y"); !strings.Contains(page, `href="/vm/test?range=24h" aria-current="page"`) {
		t.Errorf("an unknown range is not 24h:\n%s", page)
	}
	if _, page := member.page("/vm/nap"); !strings.Contains(page, "Nothing recorded yet") || strings.Contains(page, `class="line"`) {
		t.Errorf("a VM without samples:\n%s", page)
	}
}

// A member makes a VM from the panel, as with ssh boxe new: a name or a
// random one, an image, its size, and maybe a first task for its agent.
func TestPanelNewVM(t *testing.T) {
	pt := newPanelTest(t)
	pt.c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261029@base"
	pt.c.host.images = []string{"boxe/images/boxeuntu-20261029@base", "boxe/images/laravel-20261031-2@base"}
	b := pt.browser("member")
	if _, page := b.page("/"); !strings.Contains(page, `href="/new"`) {
		t.Errorf("no way to a new VM in the list:\n%s", page)
	}
	code, page := b.page("/new")
	if code != 200 {
		t.Fatalf("%d:\n%s", code, page)
	}
	for _, want := range []string{`action="/new"`, `name="name"`, `<option value="laravel">`, `name="cpu"`, `name="mem"`} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	token := csrf(t, page)

	res := b.post("/new", url.Values{"csrf": {token}, "name": {"web"}, "image": {"laravel"}, "cpu": {"4"}, "mem": {"4G"}}, fromPanel)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/vm/web" {
		t.Fatalf("new: %d %q\n%s", res.StatusCode, res.Header.Get("Location"), body(res))
	}
	if v, err := pt.c.box.store.Get("web"); err != nil || v.Owner != "alice" || v.CPUs != 4 || v.Mem != "4G" || !strings.Contains(v.Image, "laravel-20261031-2") {
		t.Errorf("web: %+v %v", v, err)
	}

	res = b.post("/new", url.Values{"csrf": {token}, "cpu": {"2"}, "mem": {"2G"}}, fromPanel)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/vm/") || loc == "/vm/" {
		t.Errorf("random name: %d %q\n%s", res.StatusCode, loc, body(res))
	}

	// Mistakes come back on the form, with what was typed.
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"name": {"web"}, "cpu": {"2"}, "mem": {"2G"}}, "A VM named web already exists."},
		{url.Values{"name": {"Bad_Name"}, "cpu": {"2"}, "mem": {"2G"}}, "Invalid name"},
		{url.Values{"name": {"big"}, "cpu": {"64"}, "mem": {"2G"}}, "between 1 and 8"},
		{url.Values{"name": {"odd"}, "image": {"nope"}, "cpu": {"2"}, "mem": {"2G"}}, "No image named"},
	} {
		c.form.Set("csrf", token)
		res := b.post("/new", c.form, fromPanel)
		page := body(res)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(page, c.want) || !strings.Contains(page, `value="`+c.form.Get("name")+`"`) {
			t.Errorf("%v: %d\n%s", c.form, res.StatusCode, page)
		}
	}

	// Only from the panel, in this session.
	fromVM := http.Header{"Sec-Fetch-Site": {"same-site"}, "Origin": {"https://test.boxe.example"}}
	if res := b.post("/new", url.Values{"csrf": {token}, "name": {"evil"}, "cpu": {"2"}, "mem": {"2G"}}, fromVM); res.StatusCode != http.StatusForbidden || pt.exists("evil") {
		t.Errorf("from a VM: %d", res.StatusCode)
	}
	if res := b.post("/new", url.Values{"name": {"evil"}, "cpu": {"2"}, "mem": {"2G"}}, fromPanel); res.StatusCode != http.StatusForbidden || pt.exists("evil") {
		t.Errorf("no token: %d", res.StatusCode)
	}

	// Someone whose email is linked to nobody cannot make VMs.
	stranger := pt.browser("stranger")
	if _, page := stranger.page("/new"); strings.Contains(page, `action="/new"`) {
		t.Errorf("a stranger gets the form:\n%s", page)
	}
}

// The owner copies a VM from its page; nobody else can, an admin included.
func TestPanelCopyVM(t *testing.T) {
	pt := newPanelTest(t)
	b := pt.browser("member")
	_, page := b.page("/vm/test")
	if !strings.Contains(page, `action="/vm/test/copy"`) {
		t.Errorf("no copy form:\n%s", page)
	}
	res := b.post("/vm/test/copy", url.Values{"csrf": {csrf(t, page)}, "name": {"test2"}}, fromPanel)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/vm/test2" {
		t.Fatalf("copy: %d %q\n%s", res.StatusCode, res.Header.Get("Location"), body(res))
	}
	if v, err := pt.c.box.store.Get("test2"); err != nil || v.Owner != "alice" {
		t.Errorf("test2: %+v %v", v, err)
	}
	res = b.post("/vm/test/copy", url.Values{"csrf": {csrf(t, page)}, "name": {"nap"}}, fromPanel)
	if page := body(res); res.StatusCode != http.StatusBadRequest || !strings.Contains(page, "A VM named nap already exists.") || !strings.Contains(page, `value="nap"`) {
		t.Errorf("copy onto nap: %d\n%s", res.StatusCode, page)
	}

	admin := pt.browser("admin")
	_, page = admin.page("/vm/test")
	if strings.Contains(page, `action="/vm/test/copy"`) {
		t.Errorf("the admin may copy alice's VM:\n%s", page)
	}
	if res := admin.post("/vm/test/copy", url.Values{"csrf": {csrf(t, page)}, "name": {"stolen"}}, fromPanel); res.StatusCode == http.StatusSeeOther || pt.exists("stolen") {
		t.Errorf("the admin copied alice's VM: %d", res.StatusCode)
	}
}

// The owner and an admin read what a VM printed on its console, since it
// was made: not what a deleted VM with the same name printed.
func TestPanelConsole(t *testing.T) {
	pt := newPanelTest(t)
	pt.c.host.journal = map[string]string{unit("test"): "[    0.000000] Linux version 7.2.8-boxe\n[\x1b[0;32m  OK  \x1b[0m] Started \x1b[0;1;39mssh.service\x1b[0m\x07\ntest login: <me>\n"}
	b := pt.browser("member")
	if _, page := b.page("/vm/test"); !strings.Contains(page, `href="/vm/test/console"`) {
		t.Errorf("no link to the console:\n%s", page)
	}
	code, page := b.page("/vm/test/console")
	if code != 200 || !strings.Contains(page, "Linux version 7.2.8-boxe") || !strings.Contains(page, "test login: &lt;me&gt;") ||
		!strings.Contains(page, "[  OK  ] Started ssh.service\n") { // no terminal colors
		t.Fatalf("%d:\n%s", code, page)
	}
	v, _ := pt.c.box.store.Get("test")
	if !pt.c.host.ran(fmt.Sprintf("journalctl _SYSTEMD_UNIT=boxe-vm-test.service --since @%d", v.Created.Unix())) {
		t.Errorf("journalctl: %v", pt.c.host.cmds)
	}
	if code, page := pt.browser("admin").page("/vm/test/console"); code != 200 || !strings.Contains(page, "Linux version") {
		t.Errorf("admin: %d", code)
	}
	if code, page := pt.browser("bob").page("/vm/test/console"); code != 404 || strings.Contains(page, "Linux version") {
		t.Errorf("bob: %d", code)
	}
	if code, _ := b.page("/vm/test/other"); code != 404 {
		t.Errorf("/vm/test/other: %d", code)
	}
	if _, page := b.page("/vm/nap/console"); !strings.Contains(page, "Nothing yet") {
		t.Errorf("empty console:\n%s", page)
	}
}
