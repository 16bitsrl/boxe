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
