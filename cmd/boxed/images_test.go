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
	out := c.must("alice", "images")
	for _, want := range []string{"boxeuntu", "boxeuntu-20261029-8", "default", "laravel-20261031-2", "old"} {
		if !strings.Contains(out, want) {
			t.Errorf("images: no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "laravel-20261030") || strings.Contains(out, "boxeuntu-20261029-4") {
		t.Errorf("images lists old versions:\n%s", out)
	}
	var list []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Default bool   `json:"default"`
	}
	c.json("alice", "images --json", &list)
	if len(list) != 3 || list[0].Name != "boxeuntu" || !list[0].Default || list[1].Version != "laravel-20261031-2" || list[1].Default {
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
