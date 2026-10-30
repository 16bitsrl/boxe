package main

import (
	"fmt"
	"log"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Images are built by admins on the host (scripts/build-base.sh), next to
// the default one: <images>/<name>-<date>[-n]@base, e.g.
// boxe/images/laravel-20261030. Names are never reused, so a new version
// gets a new date or number. Older versions go once no VM uses them.

// image is what `ssh boxe images` shows: the newest version of a name.
type image struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Default bool   `json:"default"` // new VMs start from it without --image
}

var imageVersion = regexp.MustCompile(`^(.+)-([0-9]{8})(?:-([0-9]+))?$`)

// imageParent is the dataset holding the images: the default one's parent.
func (h *Hypervisor) imageParent() string {
	return path.Dir(strings.TrimSuffix(h.BaseSnapshot, "@base"))
}

// imageVersions lists every image version on the host, e.g.
// laravel-20261030, oldest first within a name.
func (h *Hypervisor) imageVersions() ([]string, error) {
	parent := h.imageParent()
	out, err := h.exec(10*time.Second, "zfs", "list", "-t", "snapshot", "-H", "-o", "name", "-d", "2", parent)
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ds, snap, ok := strings.Cut(line, "@")
		if ok && snap == "base" && path.Dir(ds) == parent {
			versions = append(versions, path.Base(ds))
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
	return versions, nil
}

// imageOf splits a version into its name, date and number (0 if none).
func imageOf(version string) (name, date string, n int) {
	m := imageVersion.FindStringSubmatch(version)
	if m == nil {
		return version, "", 0
	}
	n, _ = strconv.Atoi(m[3])
	return m[1], m[2], n
}

func versionLess(a, b string) bool {
	an, ad, ai := imageOf(a)
	bn, bd, bi := imageOf(b)
	if an != bn {
		return an < bn
	}
	if ad != bd {
		return ad < bd
	}
	return ai < bi
}

// Images lists the newest version of each image, by name.
func (h *Hypervisor) Images() ([]image, error) {
	versions, err := h.imageVersions()
	if err != nil {
		return nil, err
	}
	images := []image{}
	for i, v := range versions {
		name, _, _ := imageOf(v)
		if i+1 < len(versions) {
			if next, _, _ := imageOf(versions[i+1]); next == name {
				continue // not the newest
			}
		}
		images = append(images, image{Name: name, Version: v, Default: h.snapshot(v) == h.BaseSnapshot})
	}
	return images, nil
}

// Image is the snapshot to clone for name: an exact version, or the
// newest version of an image.
func (h *Hypervisor) Image(name string) (string, error) {
	versions, err := h.imageVersions()
	if err != nil {
		return "", err
	}
	found := ""
	for _, v := range versions {
		if n, _, _ := imageOf(v); v == name || n == name {
			found = v
			if v == name {
				break
			}
		}
	}
	if found == "" {
		return "", fmt.Errorf("no image named %q", name)
	}
	return h.snapshot(found), nil
}

func (h *Hypervisor) snapshot(version string) string {
	return h.imageParent() + "/" + version + "@base"
}

// pruneImages deletes the image versions nobody can pick any more (not the
// default, not the newest of their name) once no VM uses them.
func (b *Box) pruneImages() {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hv
	versions, err := h.imageVersions()
	if err != nil {
		log.Printf("images: %v", err)
		return
	}
	vms, err := b.store.List("")
	if err != nil {
		log.Printf("images: %v", err)
		return
	}
	used := map[string]bool{h.BaseSnapshot: true}
	for _, v := range vms {
		used[v.Image] = true // copies too: their disk may come from the image through their source's
	}
	for i, v := range versions {
		name, _, _ := imageOf(v)
		snap := h.snapshot(v)
		if used[snap] || i+1 == len(versions) {
			continue
		}
		if next, _, _ := imageOf(versions[i+1]); next != name {
			continue // the newest of its name
		}
		out, err := h.exec(30*time.Second, "zfs", "list", "-H", "-o", "clones", snap)
		if err != nil || strings.TrimSpace(string(out)) != "-" {
			continue // a disk still comes from it, or we cannot tell
		}
		// Only an image with nothing else on it (a snapshot an admin took):
		// one without its @base would no longer be listed, nor cleaned up.
		ds := strings.TrimSuffix(snap, "@base")
		out, err = h.exec(30*time.Second, "zfs", "list", "-H", "-o", "name", "-t", "all", "-r", ds)
		if err != nil || strings.Join(strings.Fields(string(out)), " ") != ds+" "+snap {
			continue
		}
		if err := h.run("zfs", "destroy", snap); err != nil {
			log.Printf("images: %s: %v", v, err)
			continue
		}
		if err := h.run("zfs", "destroy", ds); err != nil {
			log.Printf("images: %s: %v; its @base is gone, delete %s by hand", v, err, ds)
			continue
		}
		log.Printf("images: removed %s, no VM uses it and a newer %s exists", v, name)
	}
}
