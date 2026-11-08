package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry publishes tags the way ghcr.io does: an anonymous token
// first, then the list in pages.
type fakeRegistry struct {
	*httptest.Server
	mu    sync.Mutex
	pages [][]string
	down  bool
}

func newFakeRegistry(t *testing.T, pages ...[]string) *fakeRegistry {
	r := &fakeRegistry{pages: pages}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case r.down:
			http.Error(w, "down", http.StatusBadGateway)
		case req.URL.Path == "/token":
			if req.URL.Query().Get("scope") != "repository:16bitsrl/boxeuntu:pull" || req.URL.Query().Get("service") != "ghcr.test" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"token":"anon"}`)
		case req.URL.Path != "/v2/16bitsrl/boxeuntu/tags/list":
			http.NotFound(w, req)
		case req.Header.Get("Authorization") != "Bearer anon":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.test",scope="repository:16bitsrl/boxeuntu:pull"`, r.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			page := 0
			fmt.Sscan(req.URL.Query().Get("page"), &page)
			if page+1 < len(r.pages) {
				w.Header().Set("Link", fmt.Sprintf(`</v2/16bitsrl/boxeuntu/tags/list?n=1000&page=%d>; rel="next"`, page+1))
			}
			fmt.Fprintf(w, `{"name":"16bitsrl/boxeuntu","tags":["%s"]}`, strings.Join(r.pages[page], `","`))
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *fakeRegistry) publish(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pages[len(r.pages)-1] = append(r.pages[len(r.pages)-1], tag)
}

// updatesCLI is a host on boxeuntu 20261107-2, with the official images at
// a fake registry.
func updatesCLI(t *testing.T, r *fakeRegistry) *cli {
	c := newCLI(t, "alice", "dave")
	c.box.keys.admins = map[string]bool{"dave": true}
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261107-2@base"
	c.host.images = []string{"boxe/images/boxeuntu-20261107-2@base", "boxe/images/laravel-20261107-2@base"}
	c.box.updates = &Updates{Repo: "ghcr.test/16bitsrl/boxeuntu", Registry: r.URL,
		Script: "/usr/local/sbin/boxe-build-base", Client: r.Client()}
	return c
}

// waitDownload waits for the download update started in the background.
func waitDownload(t *testing.T, c *cli) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); c.box.updates.Status().Getting != ""; {
		if time.Now().After(deadline) {
			t.Fatal("the download never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.box.updates.run.Lock() // the goroutine is done with it
	c.box.updates.run.Unlock()
}

// A newer official boxeuntu is downloaded, made an image and the default
// for new VMs; the host keeps it across restarts, and an admin can go back.
func TestOfficialImageUpdate(t *testing.T) {
	r := newFakeRegistry(t, []string{"20261105", "latest"}, []string{"20261107-2", "20261110", "sha-1a2b3c"})
	c := updatesCLI(t, r)

	if _, errOut, ok := c.run("alice", "images update"); ok || !strings.Contains(errOut, "admin") {
		t.Errorf("a member updated the images: %q", errOut)
	}
	out := c.must("dave", "images update")
	if !strings.Contains(out, "Downloading boxeuntu 20261110") {
		t.Fatalf("images update:\n%s", out)
	}
	waitDownload(t, c)
	for _, cmd := range []string{
		"docker pull -q ghcr.test/16bitsrl/boxeuntu:20261110",
		"docker tag ghcr.test/16bitsrl/boxeuntu:20261110 boxeuntu:20261110",
		"docker rmi ghcr.test/16bitsrl/boxeuntu:20261110",
		"/usr/local/sbin/boxe-build-base boxeuntu:20261110 boxe/images/boxeuntu-20261110",
		"docker rmi boxeuntu:20261110", // the image has it all: no copy left in Docker
	} {
		if !c.host.ran(cmd) {
			t.Errorf("did not run %q: %v", cmd, c.host.cmds)
		}
	}
	if out := c.must("alice", "images"); !strings.Contains(out, "boxeuntu  20261110    -  default") {
		t.Errorf("images after the update:\n%s", out)
	}
	var v vmJSON
	c.json("alice", "new a1 --json", &v)
	if v.Image != "boxeuntu-20261110" {
		t.Errorf("a new VM started from %q", v.Image)
	}
	// laravel is built here from boxeuntu: it is not the updates' business.
	c.json("alice", "new a2 --image laravel --json", &v)
	if v.Image != "laravel-20261107-2" {
		t.Errorf("laravel: %q", v.Image)
	}

	pulls := strings.Count(strings.Join(c.host.cmds, "\n"), "docker pull")
	if out := c.must("dave", "images update"); !strings.Contains(out, "Up to date: the newest boxeuntu published is 20261110") {
		t.Errorf("a second update:\n%s", out)
	}
	if strings.Count(strings.Join(c.host.cmds, "\n"), "docker pull") != pulls {
		t.Error("downloaded the same version again")
	}

	// A restart keeps the default; once an admin points -base elsewhere,
	// -base wins.
	again := &Box{store: c.box.store, hv: &Hypervisor{BaseSnapshot: c.box.hv.BaseSnapshot, Exec: c.box.hv.Exec}}
	again.loadDefault()
	if again.hv.Base() != "boxe/images/boxeuntu-20261110@base" {
		t.Errorf("after a restart new VMs start from %s", again.hv.Base())
	}
	moved := &Box{store: c.box.store, hv: &Hypervisor{BaseSnapshot: "boxe/images/boxeuntu-20261111@base", Exec: c.box.hv.Exec}}
	moved.loadDefault()
	if moved.hv.Base() != "boxe/images/boxeuntu-20261111@base" {
		t.Errorf("-base changed, yet new VMs start from %s", moved.hv.Base())
	}
}

// An admin goes back to an older version: the next updates leave it be,
// until a newer one is published.
func TestImageDefaultByHand(t *testing.T) {
	r := newFakeRegistry(t, []string{"20261107-2", "20261110"})
	c := updatesCLI(t, r)
	c.must("dave", "images update")
	waitDownload(t, c)
	// The cleanup keeps the version before, to go back to, and -base.
	c.box.pruneImages()
	if c.host.ran("zfs destroy boxe/images/boxeuntu-20261107-2") {
		t.Fatalf("the cleanup deleted the version to go back to: %v", c.host.cmds)
	}

	if _, errOut, ok := c.run("alice", "images default boxeuntu-20261107-2"); ok || !strings.Contains(errOut, "admin") {
		t.Errorf("a member changed the default: %q", errOut)
	}
	if _, errOut, ok := c.run("dave", "images default nope-20261101"); ok || !strings.Contains(errOut, "ssh boxe images") {
		t.Errorf("an unknown version: %q", errOut)
	}
	if out := c.must("dave", "images default boxeuntu-20261107-2"); !strings.Contains(out, "New VMs start from boxeuntu-20261107-2") {
		t.Errorf("images default:\n%s", out)
	}
	if c.box.hv.Base() != "boxe/images/boxeuntu-20261107-2@base" {
		t.Errorf("default %s", c.box.hv.Base())
	}
	c.must("dave", "images update")
	if c.box.hv.Base() != "boxe/images/boxeuntu-20261107-2@base" {
		t.Errorf("an update undid the admin's choice: %s", c.box.hv.Base())
	}

	r.publish("20261112")
	if _, err := c.box.updateImage(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if c.box.hv.Base() != "boxe/images/boxeuntu-20261112@base" {
		t.Errorf("a newer version did not become the default: %s", c.box.hv.Base())
	}
	// Now 20261110 is the one to go back to; 20261107-2 stays as -base.
	r.publish("20261113")
	if _, err := c.box.updateImage(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	c.box.pruneImages()
	if !c.host.ran("zfs destroy boxe/images/boxeuntu-20261110") {
		t.Errorf("kept a version neither default, -base, the one before nor in use: %v", c.host.cmds)
	}
	for _, kept := range []string{"boxeuntu-20261107-2", "boxeuntu-20261112", "boxeuntu-20261113"} {
		if c.host.ran("zfs destroy boxe/images/" + kept) {
			t.Errorf("deleted %s", kept)
		}
	}
}

// What a killed build left behind (a dataset without @base) is removed
// before the next try, instead of blocking every try after.
func TestImageUpdateAfterKilledBuild(t *testing.T) {
	r := newFakeRegistry(t, []string{"20261110"})
	c := updatesCLI(t, r)
	left := filepath.Join(c.host.devDir, "boxe/images/boxeuntu-20261110")
	os.MkdirAll(filepath.Dir(left), 0o755)
	os.WriteFile(left, nil, 0o644)
	if _, err := c.box.updateImage(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(c.host.cmds, "\n")
	destroy := strings.Index(cmds, "zfs destroy -r boxe/images/boxeuntu-20261110")
	build := strings.Index(cmds, "boxe-build-base boxeuntu:20261110")
	if destroy < 0 || build < destroy {
		t.Errorf("the leftover was not removed before the build:\n%s", cmds)
	}
}

// Before the first image is published, or while it is not public, the
// registry denies the anonymous token: nothing published, not an error.
func TestNothingPublishedYet(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			http.Error(w, `{"errors":[{"code":"DENIED"}]}`, http.StatusForbidden)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+"http://"+req.Host+`/token",service="ghcr.test",scope="repository:16bitsrl/boxeuntu:pull"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer denied.Close()
	c := updatesCLI(t, &fakeRegistry{Server: denied})
	out := c.must("dave", "images update")
	if !strings.Contains(out, "No boxeuntu published yet") {
		t.Errorf("images update:\n%s", out)
	}
	if s := c.box.hostImage(); s.Err != "" || s.Checked == "" {
		t.Errorf("host page: %+v", s)
	}
}

// A host whose default is an image of its own builds its images itself.
func TestOwnImageNotUpdated(t *testing.T) {
	r := newFakeRegistry(t, []string{"20261110"})
	c := updatesCLI(t, r)
	c.box.hv.BaseSnapshot = "boxe/images/ours-20261101@base"
	c.host.images = append(c.host.images, "boxe/images/ours-20261101@base")
	out := c.must("dave", "images update")
	if !strings.Contains(out, "The default image is ours-20261101, not boxeuntu: it is not updated") || c.host.ran("docker pull") {
		t.Errorf("updated a host on its own image:\n%s\n%v", out, c.host.cmds)
	}
}

// A registry out of reach, or a failed download, shows on the host page
// and leaves the default as it was.
func TestImageUpdateFails(t *testing.T) {
	r := newFakeRegistry(t, []string{"20261110"})
	c := updatesCLI(t, r)
	r.mu.Lock()
	r.down = true
	r.mu.Unlock()
	if _, errOut, ok := c.run("dave", "images update"); ok || !strings.Contains(errOut, "could not reach ghcr.test/16bitsrl/boxeuntu") {
		t.Errorf("registry down: %q", errOut)
	}
	if s := c.box.hostImage(); s.Err != "could not reach ghcr.test/16bitsrl/boxeuntu" {
		t.Errorf("host page: %+v", s)
	}
	r.mu.Lock()
	r.down = false
	r.mu.Unlock()

	c.host.mu.Lock()
	c.host.fail = map[string]bool{"docker pull": true}
	c.host.mu.Unlock()
	if _, err := c.box.updateImage(context.Background(), true); err == nil {
		t.Error("a failed download succeeded")
	}
	s := c.box.hostImage()
	if s.Err != "could not download 20261110" || s.Version != "20261107-2" || s.Getting != "" {
		t.Errorf("host page after a failed download: %+v", s)
	}
	if c.box.hv.Base() != "boxe/images/boxeuntu-20261107-2@base" {
		t.Errorf("default after a failed download: %s", c.box.hv.Base())
	}
}

// The admin's page of the panel says which image new VMs start from and
// how its updates go.
func TestHostPageImage(t *testing.T) {
	pt := newPanelTest(t)
	r := newFakeRegistry(t, []string{"20261110"})
	b := pt.c.box
	b.hv.BaseSnapshot = "boxe/images/boxeuntu-20261107-2@base"
	pt.c.host.mu.Lock()
	pt.c.host.images = []string{"boxe/images/boxeuntu-20261107-2@base"}
	pt.c.host.sources = map[string]string{"boxe/images/boxeuntu-20261107-2@base": "v0.1.1"}
	pt.c.host.mu.Unlock()
	b.updates = &Updates{Repo: "ghcr.test/16bitsrl/boxeuntu", Registry: r.URL, Script: "/usr/local/sbin/boxe-build-base", Client: r.Client()}

	_, page := pt.browser("admin").page("/host")
	for _, want := range []string{"boxeuntu 20261107-2, from boxe v0.1.1", "not checked yet"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %q in:\n%s", want, page)
		}
	}
	b.updates.set(func(s *updateStatus) { s.Getting = "boxeuntu-20261110" })
	if _, page = pt.browser("admin").page("/host"); !strings.Contains(page, "downloading boxeuntu 20261110") {
		t.Errorf("no download in:\n%s", page)
	}
	b.updates.set(func(s *updateStatus) { s.Getting, s.Checked, s.Published = "", time.Now(), "boxeuntu-20261110" })
	if _, page = pt.browser("admin").page("/host"); !strings.Contains(page, "newest published 20261110") {
		t.Errorf("no check in:\n%s", page)
	}
	b.updates = nil
	if _, page = pt.browser("admin").page("/host"); !strings.Contains(page, "<dt>Updates</dt><dd>off") {
		t.Errorf("updates off:\n%s", page)
	}
}
