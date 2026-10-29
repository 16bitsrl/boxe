package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fork copies a running VM with its memory: disk and memory are saved
// together while the source is paused for a moment, then the copy is
// restored from them and takes its own identity.
func TestForkCopiesARunningVM(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	v := VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}
	f.cmds = nil
	if err := h.Fork(src, v, ""); err != nil {
		t.Fatal(err)
	}
	s := sock("src")
	order := []string{"ch-remote --api-socket " + s + " pause", "zfs snapshot boxe/vms/src@cp-copy-",
		"ch-remote --api-socket " + s + " snapshot", "ch-remote --api-socket " + s + " resume",
		"zfs clone boxe/vms/src@cp-copy-", "systemd-run -q --unit " + unit("copy")}
	i := 0
	for _, c := range f.cmds {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i < len(order) {
		t.Errorf("missing or out of order: %q\nran:\n%s", order[i], strings.Join(f.cmds, "\n"))
	}
	if h.State("src") != "running" || h.State("copy") != "running" {
		t.Errorf("states: src %s, copy %s", h.State("src"), h.State("copy"))
	}
	if !strings.Contains(strings.Join(f.agent, "\n"), "copy reconfigure") {
		t.Errorf("the copy was not told to take its identity: %v", f.agent)
	}
	for _, dir := range []string{h.sleepDir("src"), h.sleepDir("copy")} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: snapshot left behind", dir)
		}
	}
}

// The copy is restored with its own disks and network, not the source's.
func TestForkUsesItsOwnDevices(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	f.fail["systemd-run -q --unit "+unit("copy")] = true
	v := VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}
	// The restore fails: look at what it was given before it is cleaned up.
	var cfg []byte
	h.Exec = func(d time.Duration, name string, args ...string) ([]byte, error) {
		if name == "systemd-run" && cfg == nil {
			cfg, _ = os.ReadFile(filepath.Join(h.sleepDir("copy"), "config.json"))
		}
		return f.exec(d, name, args...)
	}
	if err := h.Fork(src, v, ""); err == nil {
		t.Fatal("no error")
	}
	var c struct {
		Disks []struct{ Path string }
		Net   []struct{ Tap, MAC string }
		Vsock struct{ Socket string }
	}
	json.Unmarshal(cfg, &c)
	if len(c.Disks) != 2 || !strings.HasSuffix(c.Disks[0].Path, "boxe/vms/copy") || c.Disks[1].Path != h.config("copy") ||
		c.Net[0].Tap != "tap3" || c.Net[0].MAC != v.MAC() || c.Vsock.Socket != vsockPath("copy") {
		t.Errorf("restored with %+v", c)
	}
	// And nothing is left of it.
	if exists(filepath.Join(f.devDir, h.ds("copy"))) || exists(h.config("copy")) {
		t.Error("a failed fork left its disks behind")
	}
	if h.State("src") != "running" {
		t.Error("the source stopped")
	}
}

func TestForkResumesTheSourceOnFailure(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	f.fail["ch-remote --api-socket "+sock("src")+" snapshot"] = true
	f.cmds = nil
	if err := h.Fork(src, VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}, ""); err == nil {
		t.Fatal("no error")
	}
	if !f.ran("ch-remote --api-socket " + sock("src") + " resume") {
		t.Error("the source was left paused")
	}
	if f.ran("zfs clone") {
		t.Error("cloned anyway")
	}
	if entries, _ := os.ReadDir(h.sleepDir("src")); len(entries) > 0 {
		t.Error("half a snapshot left in the source's directory")
	}
}

// VMs from an image without boxe-agent cannot take a new identity: they
// are not forked, and the user is told to copy them instead.
func TestForkNeedsTheAgent(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	f.noAgent = true
	err := h.Fork(src, VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}, "")
	if err == nil || !strings.Contains(err.Error(), "cp") {
		t.Fatalf("got %v, want a hint to use cp", err)
	}
	if f.ran("ch-remote --api-socket " + sock("src") + " pause") {
		t.Error("paused the source anyway")
	}
}

// A copy that cannot take its identity would answer as its source: it is
// deleted.
func TestForkFailsIfTheCopyKeepsTheSourceIdentity(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	f.agentErr["reconfigure"] = "could not set up the network"
	if err := h.Fork(src, VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}, ""); err == nil {
		t.Fatal("no error")
	}
	if h.Running("copy") || exists(filepath.Join(f.devDir, h.ds("copy"))) {
		t.Error("the copy is still there")
	}
}

func TestForkCommand(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test --mem 1G")
	if _, _, ok := c.run("bob", "fork test mine"); ok {
		t.Error("bob forked alice's VM")
	}
	var v vmJSON
	c.json("alice", "fork test copy --json", &v)
	if v.Name != "copy" || v.State != "running" || v.Mem != "1G" {
		t.Errorf("fork --json: %+v", v)
	}
	// A sleeping VM wakes up to be forked.
	c.must("alice", "sleep test")
	if out := c.must("alice", "fork test copy2"); !strings.Contains(out, "copy2 ready in") {
		t.Errorf("fork of a sleeping VM: %q", out)
	}
	if s := c.box.hv.State("test"); s != "running" {
		t.Errorf("source %s after the fork", s)
	}
}

// A copy restored from a freshly started source may get its first
// connection mixed up with one the source had not finished closing, which
// then cuts it off. The copy is pinged until a connection gets through, and
// told to reconfigure only once: a cut off reconfigure may still be running.
func TestForkWaitsForAClearConnection(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := runningVM(t, h, "src")
	f.hangUp["copy ping"] = 2
	if err := h.Fork(src, VM{Name: "copy", Index: 3, CPUs: 1, Mem: "1G"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.Join(f.agent, "\n"), "copy reconfigure"); got != 1 {
		t.Errorf("reconfigured %d times", got)
	}
	f.hangUp["copy2 reconfigure"] = 1
	if err := h.Fork(src, VM{Name: "copy2", Index: 4, CPUs: 1, Mem: "1G"}, ""); err == nil {
		t.Error("a cut off reconfigure was taken for done")
	}
	if got := strings.Count(strings.Join(f.agent, "\n"), "copy2 reconfigure"); got != 1 {
		t.Errorf("reconfigured %d times", got)
	}
}
