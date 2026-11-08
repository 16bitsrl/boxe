package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Official images: the boxe project publishes boxeuntu to a registry
// (-image-updates, e.g. ghcr.io/16bitsrl/boxeuntu, tags <date>[-n]). Every
// day boxed looks there for a version newer than every boxeuntu on the host,
// downloads it, makes an image of it (build-base.sh) and the default for new
// VMs. An installation whose default is an image of its own keeps it: it
// builds its images itself.

// The default an update or an admin chose, and the -base it was chosen
// over: once an admin points -base somewhere else, -base wins again.
const (
	defaultImageKey = "default_image"
	defaultBaseKey  = "default_image_over"
)

// Updates downloads the official images.
type Updates struct {
	Repo     string // e.g. ghcr.io/16bitsrl/boxeuntu
	Registry string // its base URL; "": https://<host of Repo>
	Script   string // build-base.sh, as installed on the host
	Client   *http.Client

	run    sync.Mutex // one check or download at a time
	mu     sync.Mutex
	status updateStatus
}

// updateStatus is what the host page shows about the updates.
type updateStatus struct {
	Checked   time.Time // the last check that reached the registry
	Published string    // the newest version there, e.g. boxeuntu-20261110
	Getting   string    // the version being downloaded
	Note      string    // why the updates do nothing here
	Err       string    // the last check or download failed
}

func (u *Updates) Status() updateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updates) set(change func(*updateStatus)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	change(&u.status)
}

// Name is the image the updates are for, e.g. boxeuntu.
func (u *Updates) Name() string { return path.Base(u.Repo) }

// errNotPublished: the registry has no such image, or not a public one.
var errNotPublished = errors.New("not published")

var tagVersion = regexp.MustCompile(`^[0-9]{8}(-[0-9]+)?$`)

// published lists the versions in the registry, oldest first, as image
// versions (boxeuntu-20261110).
func (u *Updates) published(ctx context.Context) ([]string, error) {
	host, repo, ok := strings.Cut(u.Repo, "/")
	if !ok {
		return nil, fmt.Errorf("-image-updates %q: want <registry>/<repository>", u.Repo)
	}
	base := u.Registry
	if base == "" {
		base = "https://" + host
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	next := base + "/v2/" + repo + "/tags/list?n=1000"
	var token string
	var versions []string
	for pages := 0; next != "" && pages < 100; pages++ {
		resp, err := u.get(ctx, next, &token)
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Repo, err)
		}
		for _, t := range body.Tags {
			if tagVersion.MatchString(t) {
				versions = append(versions, u.Name()+"-"+t)
			}
		}
		next = ""
		if ref, _, ok := strings.Cut(link, ">"); ok && strings.Contains(link, `rel="next"`) {
			if r, err := url.Parse(strings.TrimPrefix(strings.TrimSpace(ref), "<")); err == nil {
				next = baseURL.ResolveReference(r).String()
			}
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
	return versions, nil
}

// get fetches from the registry, with the anonymous token it asks for.
func (u *Updates) get(ctx context.Context, target string, token *string) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		resp, err := u.Client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if *token, err = u.token(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
				return nil, errNotPublished
			}
			return nil, fmt.Errorf("%s: %s", u.Repo, resp.Status)
		}
		return resp, nil
	}
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// token answers a Bearer challenge (WWW-Authenticate) without credentials:
// public images need none.
func (u *Updates) token(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%s: the registry wants %q", u.Repo, scheme)
	}
	p := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(params, -1) {
		p[m[1]] = m[2]
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("%s: no token service", u.Repo)
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return "", errNotPublished // ghcr.io, for a package that is missing or private
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: token: %s", u.Repo, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("%s: token: %w", u.Repo, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

// loadDefault puts back the default an update or an admin chose, unless an
// admin has pointed -base somewhere else since, or its image is gone.
func (b *Box) loadDefault() {
	snap, err := b.store.Setting(defaultImageKey)
	if err != nil || snap == "" {
		return
	}
	if over, _ := b.store.Setting(defaultBaseKey); over != b.hv.BaseSnapshot {
		log.Printf("images: -base changed, new VMs start from it again")
		b.store.SetSetting(defaultImageKey, "")
		return
	}
	versions, err := b.hv.imageVersions()
	if err != nil {
		log.Printf("images: %v; new VMs start from -base", err)
		return
	}
	for _, v := range versions {
		if b.hv.snapshot(v) == snap {
			b.hv.SetBase(snap)
			return
		}
	}
	log.Printf("images: the default %s is gone, new VMs start from -base", snap)
}

// setDefault makes snapshot the default image, across restarts.
func (b *Box) setDefault(snapshot string) error {
	b.mu.Lock() // against a cleanup deciding what to keep, and a new VM
	defer b.mu.Unlock()
	if err := b.store.SetSetting(defaultBaseKey, b.hv.BaseSnapshot); err != nil {
		return err
	}
	if err := b.store.SetSetting(defaultImageKey, snapshot); err != nil {
		return err
	}
	b.hv.SetBase(snapshot)
	return nil
}

// UpdateLoop checks for a new official image every so often, sooner again
// after a failure.
func (b *Box) UpdateLoop(first, every time.Duration) {
	time.Sleep(first)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		msg, err := b.updateImage(ctx, true)
		cancel()
		wait := every
		// An admin's download may still be running, and fail.
		if err != nil || strings.HasPrefix(msg, "Already") {
			wait = min(every, time.Hour)
		}
		time.Sleep(wait)
	}
}

// updateImage looks for an official image newer than the host's and
// downloads it: before returning with wait, else in the background. It
// says what it found or did.
func (b *Box) updateImage(ctx context.Context, wait bool) (string, error) {
	u := b.updates
	if !u.run.TryLock() {
		if g := u.Status().Getting; g != "" {
			return fmt.Sprintf("Already downloading %s.\n", g), nil
		}
		return "Already checking for a new image.\n", nil
	}
	unlock := true
	defer func() {
		if unlock {
			u.run.Unlock()
		}
	}()
	versions, err := u.published(ctx)
	if errors.Is(err, errNotPublished) {
		versions, err = nil, nil
	}
	if err != nil {
		log.Printf("images: updates: %v", err)
		u.set(func(s *updateStatus) { s.Err = "could not reach " + u.Repo })
		return "", hostErrorf("could not reach %s", u.Repo)
	}
	newest := ""
	if len(versions) > 0 {
		newest = versions[len(versions)-1]
	}
	def := path.Base(strings.TrimSuffix(b.hv.Base(), "@base"))
	note := ""
	if n, _, _ := imageOf(def); n != u.Name() {
		note = fmt.Sprintf("the default image is %s, not %s: it is not updated", def, u.Name())
	}
	u.set(func(s *updateStatus) { s.Checked, s.Published, s.Note, s.Err = time.Now(), newest, note, "" })
	if newest == "" {
		return fmt.Sprintf("No %s published yet at %s (or not public).\n", u.Name(), u.Repo), nil
	}
	if note != "" {
		return strings.ToUpper(note[:1]) + note[1:] + ".\n", nil
	}
	local, err := b.hv.imageVersions()
	if err != nil {
		log.Printf("images: %v", err)
		return "", hostErrorf("could not list the images")
	}
	for _, v := range local {
		if n, _, _ := imageOf(v); n == u.Name() && !versionLess(v, newest) {
			return fmt.Sprintf("Up to date: the newest %s published is %s.\n", u.Name(), shortVersion(newest)), nil
		}
	}

	u.set(func(s *updateStatus) { s.Getting = newest })
	get := func() error {
		err := b.getImage(newest)
		u.set(func(s *updateStatus) {
			s.Getting = ""
			if err != nil {
				s.Err = "could not download " + shortVersion(newest)
			}
		})
		if err != nil {
			log.Printf("images: %s: %v", newest, err)
			return hostErrorf("could not download %s %s", u.Name(), shortVersion(newest))
		}
		return nil
	}
	if wait {
		if err := get(); err != nil {
			return "", err
		}
		return fmt.Sprintf("Downloaded %s %s: new VMs start from it.\n", u.Name(), shortVersion(newest)), nil
	}
	unlock = false
	go func() {
		defer u.run.Unlock()
		get()
	}()
	return fmt.Sprintf("Downloading %s %s: once it is ready, new VMs start from it (ssh %s images shows it).\n",
		u.Name(), shortVersion(newest), b.name), nil
}

// getImage downloads a published version, makes an image of it and the
// default for new VMs.
func (b *Box) getImage(version string) error {
	u, h := b.updates, b.hv
	tag := shortVersion(version)
	remote, local := u.Repo+":"+tag, u.Name()+":"+tag
	if _, err := h.exec(time.Hour, "docker", "pull", "-q", remote); err != nil {
		return err
	}
	// Under the name an admin's build has, which pruneBuilds cleans up.
	if err := h.run("docker", "tag", remote, local); err != nil {
		return err
	}
	if err := h.run("docker", "rmi", remote); err != nil {
		log.Printf("images: %v", err)
	}
	ds := h.imageParent() + "/" + version
	// What a try stopped half way left: a dataset without @base, which is
	// no image (it is not one of imageVersions, or we would not be here).
	if _, err := h.exec(10*time.Second, "zfs", "list", "-H", "-o", "name", ds); err == nil {
		log.Printf("images: removing %s, left by a download that did not finish", ds)
		if err := h.run("zfs", "destroy", "-r", ds); err != nil {
			return err
		}
	}
	if err := h.script(2*time.Hour, u.Script, local, ds); err != nil {
		return err
	}
	// The image has it all; build images FROM it pull it again.
	if err := h.run("docker", "rmi", local); err != nil {
		log.Printf("images: %v", err)
	}
	if err := b.setDefault(ds + "@base"); err != nil {
		return err
	}
	log.Printf("images: downloaded %s, new VMs start from it", version)
	return nil
}

// script runs a host script that cleans up after itself when stopped: at
// its deadline it and its children get SIGTERM, and time to clean up.
func (h *Hypervisor) script(d time.Duration, name string, args ...string) error {
	if h.Exec != nil {
		_, err := h.Exec(d, name, args...)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var out capped
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = &out, &out
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGTERM) }
	c.WaitDelay = 5 * time.Minute
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out.b)))
	}
	return nil
}

// shortVersion is a version without its image's name: 20261110-2.
func shortVersion(version string) string {
	name, date, _ := imageOf(version)
	if date == "" {
		return version
	}
	return strings.TrimPrefix(version, name+"-")
}
