package main

import (
	"fmt"
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
// gets a new date or number.

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
