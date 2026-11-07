package main

import (
	"strings"
	"testing"
)

// Admins build images on the host (<name>-<date>[-n]); `ssh boxe images`
// lists them and `new --image <name>` starts from the newest of that name,
// or from an exact version.
func TestImages(t *testing.T) {
	c := newCLI(t, "alice")
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261029-8@base"
	c.host.images = []string{
		"boxe/images/boxeuntu-20261029-4@base",
		"boxe/images/boxeuntu-20261029-8@base",
		"boxe/images/boxeuntu-20261029@base",
		"boxe/images/laravel-20261030@base",
		"boxe/images/laravel-20261030@before-php",
		"boxe/images/laravel-20261031-2@base",
		"boxe/images/old@base",
	}
	c.host.sources = map[string]string{"boxe/images/boxeuntu-20261029-8@base": "v0.1.0-2-gcd9f842"}
	// The version without the name again; the boxe it was built from, when
	// build-base.sh recorded it.
	out := c.must("alice", "images")
	want := "NAME      VERSION     BUILT FROM\n" +
		"boxeuntu  20261029-8  v0.1.0-2-gcd9f842  default\n" +
		"laravel   20261031-2  -\n" +
		"old       -           -\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("images:\n%s\nwant\n%s", out, want)
	}
	var list []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Source  string `json:"source"`
		Default bool   `json:"default"`
	}
	c.json("alice", "images --json", &list)
	if len(list) != 3 || list[0].Name != "boxeuntu" || !list[0].Default || list[0].Source != "v0.1.0-2-gcd9f842" ||
		list[1].Version != "laravel-20261031-2" || list[1].Default || list[1].Source != "" {
		t.Errorf("images --json: %+v", list)
	}

	var v vmJSON
	c.json("alice", "new a1 --image laravel --json", &v)
	if v.Image != "laravel-20261031-2" || !c.host.ran("zfs clone boxe/images/laravel-20261031-2@base boxe/vms/a1") {
		t.Errorf("new --image laravel: image %q", v.Image)
	}
	c.json("alice", "new a2 --json --image laravel-20261030", &v)
	if v.Image != "laravel-20261030" {
		t.Errorf("exact version: image %q", v.Image)
	}
	c.json("alice", "cp a1 a3 --json", &v)
	if v.Image != "laravel-20261031-2" {
		t.Errorf("a copy changed image: %q", v.Image)
	}
	if _, errOut, ok := c.run("alice", "new a4 --image nope"); ok || !strings.Contains(errOut, "ssh boxe images") {
		t.Errorf("unknown image: %q", errOut)
	}
	if _, errOut, ok := c.run("alice", "new a4 --image ../../vms/a1"); ok || !strings.Contains(errOut, "no image") {
		t.Errorf("image outside the images: %q", errOut)
	}
	if _, _, ok := c.run("alice", "new a4 --image"); ok {
		t.Error("--image without a name succeeded")
	}
	if _, err := c.box.store.Get("a4"); err == nil {
		t.Error("a VM was recorded for a failed new")
	}
}

// An image version nobody can pick any more (not the default, not the newest
// of its name) is deleted once its last VM is gone, so the host does not
// fill up with old images.
func TestOldImagesGoWithTheirLastVM(t *testing.T) {
	c := newCLI(t, "alice")
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261029@base"
	c.host.images = []string{
		"boxe/images/boxeuntu-20261029@base",
		"boxe/images/boxeuntu-20261030@base",
		"boxe/images/laravel-20261030@base",
	}
	c.must("alice", "new a1 --image laravel")
	c.must("alice", "cp a1 a2")
	c.host.images = append(c.host.images, "boxe/images/laravel-20261031@base") // an admin builds a new version

	c.box.pruneImages()
	if c.host.ran("zfs destroy boxe/images/") {
		t.Fatalf("an image was deleted while VMs use it or new ones can pick it: %v", c.host.cmds)
	}

	c.must("alice", "rm a1")
	c.box.pruneImages()
	if c.host.ran("zfs destroy boxe/images/laravel-20261030") {
		t.Fatal("deleted the image of a2, a copy of a1")
	}

	c.must("alice", "rm a2")
	c.box.pruneImages()
	if !c.host.ran("zfs destroy boxe/images/laravel-20261030@base") || !c.host.ran("zfs destroy boxe/images/laravel-20261030") {
		t.Errorf("the old laravel image was kept: %v", c.host.cmds)
	}
	for _, kept := range []string{"boxeuntu-20261029", "boxeuntu-20261030", "laravel-20261031"} {
		if c.host.ran("zfs destroy boxe/images/" + kept) {
			t.Errorf("deleted %s, the default or the newest of its name", kept)
		}
	}
	out := c.must("alice", "images")
	if strings.Contains(out, "laravel-20261030") {
		t.Errorf("images still lists the deleted version:\n%s", out)
	}
}

// An old image with anything else on it, such as a snapshot an admin took,
// is left whole: deleting its @base alone would hide it from images and
// from every later cleanup, its space still taken.
func TestOldImageWithMoreOnItStays(t *testing.T) {
	c := newCLI(t, "alice")
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261030@base"
	c.host.images = []string{
		"boxe/images/boxeuntu-20261030@base",
		"boxe/images/laravel-20261030@base",
		"boxe/images/laravel-20261030@before-maintenance",
		"boxe/images/laravel-20261031@base",
	}
	c.box.pruneImages()
	if c.host.ran("zfs destroy boxe/images/laravel-20261030") {
		t.Errorf("destroyed part of an image with an admin's snapshot on it: %v", c.host.cmds)
	}
}

// The Docker images an admin built them from go too, once their image is
// gone and a newer one of the same name exists: one being built (newer than
// any image, or with an image still to come) and anything else stay.
func TestOldBuildsGoWithTheirImage(t *testing.T) {
	c := newCLI(t, "alice", "dave")
	c.box.keys.admins = map[string]bool{"dave": true}
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261107-2@base"
	c.host.images = []string{
		"boxe/images/boxeuntu-20261105-3@base",
		"boxe/images/boxeuntu-20261107-2@base",
		"boxe/images/laravel-20261107-2@base",
	}
	c.must("alice", "new a1 --image boxeuntu-20261105-3")
	c.host.builds = []string{
		"ghcr.io/boldsoftware/exeuntu:latest",
		"<none>:<none>",
		"boxeuntu:20261030",
		"boxeuntu:20261105-3",
		"boxeuntu:20261107",
		"boxeuntu:20261107-2",
		"boxeuntu:20261108",
		"laravel:20261107",
		"laravel:20261107-2",
		"other:20261101",
		"boxeuntu:latest",
	}

	if _, errOut, ok := c.run("alice", "images prune"); ok || !strings.Contains(errOut, "admin") {
		t.Errorf("a member pruned the images: %q", errOut)
	}
	if c.host.ran("docker rmi") {
		t.Fatalf("a member's prune removed builds: %v", c.host.cmds)
	}

	out := c.must("dave", "images prune")
	for _, gone := range []string{"boxeuntu:20261030", "boxeuntu:20261107", "laravel:20261107"} {
		if !c.host.ran("docker rmi " + gone) {
			t.Errorf("kept %s, whose image is gone", gone)
		}
		if !strings.Contains(out, gone) {
			t.Errorf("prune does not say it removed %s:\n%s", gone, out)
		}
	}
	want := []string{"ghcr.io/boldsoftware/exeuntu:latest", "<none>:<none>", "boxeuntu:20261105-3",
		"boxeuntu:20261107-2", "boxeuntu:20261108", "laravel:20261107-2", "other:20261101", "boxeuntu:latest"}
	if strings.Join(c.host.builds, " ") != strings.Join(want, " ") {
		t.Errorf("builds left: %v\nwant %v", c.host.builds, want)
	}
	if c.host.ran("zfs destroy") {
		t.Errorf("deleted an image in use or the newest: %v", c.host.cmds)
	}

	// Once its last VM is gone, the old image goes, and its build with it.
	c.must("alice", "rm a1")
	out = c.must("dave", "images prune")
	if !c.host.ran("zfs destroy boxe/images/boxeuntu-20261105-3@base") || !c.host.ran("docker rmi boxeuntu:20261105-3") ||
		!strings.Contains(out, "boxeuntu-20261105-3") {
		t.Errorf("the old image or its build stayed:\n%s\n%v", out, c.host.cmds)
	}
	if out = c.must("dave", "images prune"); !strings.Contains(out, "Nothing to remove") {
		t.Errorf("prune with nothing left:\n%s", out)
	}
}

// A host without Docker (images built elsewhere) prunes its images all the
// same.
func TestPruneWithoutDocker(t *testing.T) {
	c := newCLI(t, "alice")
	c.box.hv.BaseSnapshot = "boxe/images/boxeuntu-20261107@base"
	c.host.images = []string{"boxe/images/boxeuntu-20261105@base", "boxe/images/boxeuntu-20261107@base"}
	c.host.fail = map[string]bool{"docker": true}
	images, builds := c.box.pruneImages()
	if len(images) != 1 || images[0] != "boxeuntu-20261105" || len(builds) != 0 {
		t.Errorf("pruned %v %v", images, builds)
	}
}
