package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A host missing a piece stops boxed at start, saying which, instead of
// failing every new VM with a vague error.
func TestCheckSetup(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	os.WriteFile(kernel, []byte("kernel"), 0o644)
	installed := map[string]bool{"/usr/local/bin/cloud-hypervisor": true, "ch-remote": true}
	lookPath := func(name string) (string, error) {
		if installed[name] {
			return name, nil
		}
		return "", errors.New("not found")
	}
	h := &Hypervisor{Kernel: kernel, SleepDir: "/boxe/sleep"}
	if err := h.checkSetup(lookPath); err != nil {
		t.Fatalf("complete host: %v", err)
	}
	for _, c := range []struct {
		name string
		h    Hypervisor
		gone string
		want string
	}{
		{"no kernel", Hypervisor{Kernel: kernel + "-7.2.8-boxe"}, "", "-kernel"},
		{"no cloud-hypervisor", Hypervisor{Kernel: kernel}, "/usr/local/bin/cloud-hypervisor", "cloud-hypervisor"},
		{"no ch-remote", Hypervisor{Kernel: kernel, SleepDir: "/boxe/sleep"}, "ch-remote", "ch-remote"},
	} {
		installed[c.gone] = false
		err := c.h.checkSetup(lookPath)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.gone != "" {
			installed[c.gone] = true
		}
	}
	// Without sleep (and so without fork) ch-remote is not needed.
	installed["ch-remote"] = false
	if err := (&Hypervisor{Kernel: kernel}).checkSetup(lookPath); err != nil {
		t.Errorf("no sleep: %v", err)
	}
}
