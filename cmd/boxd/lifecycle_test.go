package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost stands in for the commands boxd runs on exe-host (zfs, systemctl,
// ip, nft, ssh): it records them and fails the ones it is told to.
type fakeHost struct {
	mu      sync.Mutex
	cmds    []string
	fail    map[string]bool // command prefix -> fails
	running map[string]bool // unit -> active
	failed  map[string]bool // unit -> failed
	devDir  string
}

func (f *fakeHost) exec(_ time.Duration, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.cmds = append(f.cmds, cmd)
	for p := range f.fail {
		if strings.HasPrefix(cmd, p) {
			return []byte("failed"), errors.New("exit status 1")
		}
	}
	switch {
	case strings.HasPrefix(cmd, "systemctl is-active"):
		if f.running[args[len(args)-1]] {
			return []byte("active\n"), nil
		}
		if f.failed[args[len(args)-1]] {
			return []byte("failed\n"), errors.New("exit status 3")
		}
		return []byte("inactive\n"), errors.New("exit status 3")
	case strings.HasPrefix(cmd, "systemctl stop"):
		delete(f.running, args[len(args)-1])
	case strings.HasPrefix(cmd, "systemd-run"):
		for i, a := range args {
			if a == "--unit" {
				f.running[args[i+1]] = true
			}
		}
	case strings.HasPrefix(cmd, "zfs clone"):
		ds := args[len(args)-1]
		os.MkdirAll(filepath.Dir(filepath.Join(f.devDir, ds)), 0o755)
		os.WriteFile(filepath.Join(f.devDir, ds), nil, 0o600)
	case strings.HasPrefix(cmd, "zfs list -H -o name"):
		if _, err := os.Stat(filepath.Join(f.devDir, args[len(args)-1])); err != nil {
			return []byte("dataset does not exist"), errors.New("exit status 1")
		}
	case strings.HasPrefix(cmd, "zfs list"):
		return []byte("-\n"), nil
	case strings.HasPrefix(cmd, "zfs destroy -r"):
		os.Remove(filepath.Join(f.devDir, args[len(args)-1]))
	}
	return nil, nil
}

func (f *fakeHost) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func newFakeHypervisor(t *testing.T) (*Hypervisor, *fakeHost) {
	t.Helper()
	ca, err := loadHostKey(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{fail: map[string]bool{}, running: map[string]bool{}, failed: map[string]bool{}, devDir: t.TempDir()}
	h := &Hypervisor{BaseSnapshot: "exe/images/base@base", Dataset: "exe/vms", Kernel: "vmlinux",
		ConfigDir: t.TempDir(), HostCA: ca, DevDir: f.devDir, Exec: f.exec}
	return h, f
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestDestroyKeepsConfigUntilDiskIsGone(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := VM{Name: "test", Index: 2, CPUs: 1, Mem: "1G"}
	if err := h.Create(v, h.BaseSnapshot, ""); err != nil {
		t.Fatal(err)
	}
	f.fail["zfs destroy"] = true
	if err := h.Destroy(v); err == nil {
		t.Fatal("Destroy succeeded although the disk could not be deleted")
	}
	if !exists(h.config(v.Name)) {
		t.Fatal("the config disk is gone while the VM's disk is still there")
	}
	delete(f.fail, "zfs destroy")
	if err := h.Destroy(v); err != nil {
		t.Fatalf("second try: %v", err)
	}
	if exists(h.config(v.Name)) {
		t.Error("config disk left behind")
	}
	// Idempotent: nothing left to delete is fine.
	if err := h.Destroy(v); err != nil {
		t.Errorf("third try: %v", err)
	}
}

func TestDestroyRefusesWhileStillRunning(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := VM{Name: "test", Index: 2, CPUs: 1, Mem: "1G"}
	if err := h.Create(v, h.BaseSnapshot, ""); err != nil {
		t.Fatal(err)
	}
	f.fail["systemctl stop"] = true
	if err := h.Destroy(v); err == nil {
		t.Fatal("Destroy succeeded although the VM is still running")
	}
	if f.ran("zfs destroy") {
		t.Error("deleted the disk of a running VM")
	}
}

func TestCreateRollsBack(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := VM{Name: "test", Index: 2, CPUs: 1, Mem: "1G"}
	f.fail["systemd-run"] = true
	err := h.Create(v, h.BaseSnapshot, "")
	if err == nil || errors.Is(err, errLeftBehind) {
		t.Fatalf("got %v, want a clean failure", err)
	}
	if exists(h.config(v.Name)) || exists(filepath.Join(f.devDir, h.ds(v.Name))) {
		t.Error("a failed create left its disks behind")
	}
}

func TestCreateReportsWhatItLeftBehind(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := VM{Name: "test", Index: 2, CPUs: 1, Mem: "1G"}
	f.fail["systemd-run"] = true
	f.fail["zfs destroy"] = true
	if err := h.Create(v, h.BaseSnapshot, ""); !errors.Is(err, errLeftBehind) {
		t.Fatalf("got %v, want errLeftBehind", err)
	}
}

func TestCopyFailsWhenTheSourceCannotBeFlushed(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := VM{Name: "src", Index: 2, CPUs: 1, Mem: "1G"}
	if err := h.Create(src, h.BaseSnapshot, ""); err != nil {
		t.Fatal(err)
	}
	f.fail["ssh"] = true
	if err := h.Copy(src, VM{Name: "dst", Index: 3, CPUs: 1, Mem: "1G"}, ""); err == nil {
		t.Fatal("copied a running VM whose disk could not be flushed")
	}
	if f.ran("zfs snapshot") {
		t.Error("took a snapshot anyway")
	}
}

// The hypervisor runs confined, with limits: a VM cannot take more than it
// was given, and its console goes to the journal, which has a size cap.
func TestStartConfinesTheHypervisor(t *testing.T) {
	h, f := newFakeHypervisor(t)
	h.VMUser = "exe-vm"
	v := VM{Name: "test", Index: 2, CPUs: 2, Mem: "2G"}
	h.writeConfig(v, "")
	os.MkdirAll(filepath.Join(f.devDir, "exe/vms"), 0o755)
	os.WriteFile(filepath.Join(f.devDir, h.ds(v.Name)), nil, 0o600)
	h.Start(v)
	var run string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "systemd-run") {
			run = c
		}
	}
	for _, want := range []string{"-p User=exe-vm", "-p NoNewPrivileges=yes", "-p ProtectSystem=strict",
		"-p MemoryMax=", "-p CPUQuota=200%", "-p Restart=on-failure", "--serial tty", "-p CapabilityBoundingSet="} {
		if !strings.Contains(run, want) {
			t.Errorf("systemd-run lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "/var/log/exe") {
		t.Error("console still goes to an unbounded file")
	}
	if !f.ran("ip tuntap add tap2 mode tap user exe-vm") {
		t.Error("tap not owned by the VM user")
	}
}

func TestAdmission(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{store: s, limits: Limits{VMMem: 8 << 10, UserMem: 6 << 10, UserVMs: 3}}
	for i, mem := range []string{"2G", "2G"} {
		if err := b.admit("u", mem); err != nil {
			t.Fatalf("vm %d: %v", i, err)
		}
		s.Insert(&VM{Name: fmt.Sprintf("v%d", i), Owner: "u", CPUs: 1, Mem: mem})
	}
	if err := b.admit("u", "4G"); err == nil {
		t.Error("admitted 8G for a user with a 6G budget")
	}
	if err := b.admit("other", "4G"); err != nil {
		t.Errorf("budgets are per user: %v", err)
	}
	if err := b.admit("u", "16G"); err == nil {
		t.Error("admitted a VM above the per-VM limit")
	}
	s.Insert(&VM{Name: "v3", Owner: "u", CPUs: 1, Mem: "512M"})
	if err := b.admit("u", "512M"); err == nil {
		t.Error("admitted a 4th VM with a limit of 3")
	}
}

func TestMemMiB(t *testing.T) {
	for in, want := range map[string]int{"512M": 512, "2G": 2048, "1g": -1, "0G": -1, "G": -1, "99999999G": -1} {
		got, err := memMiB(in)
		if want < 0 {
			if err == nil {
				t.Errorf("%s: want error, got %d", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: got %d %v, want %d", in, got, err, want)
		}
	}
}

func TestRandomNameGivesUp(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{store: s}
	for _, a := range adjectives {
		for _, n := range nouns {
			s.Insert(&VM{Name: a + "-" + n, Owner: "u", CPUs: 1, Mem: "1G"})
		}
	}
	done := make(chan error)
	go func() { _, err := b.randomName(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("returned a name although all are taken")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("randomName loops forever when all names are taken")
	}
}

func TestHealth(t *testing.T) {
	h, f := newFakeHypervisor(t)
	s, err := openStore(filepath.Join(t.TempDir(), "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b := &Box{store: s, hv: h, keys: &Keys{store: s}, backupConf: filepath.Join(dir, "backup.conf"), backupLast: filepath.Join(dir, "backup-last")}
	b.keys.lastOK = time.Now()
	s.Insert(&VM{Name: "a", Owner: "u", CPUs: 1, Mem: "1G"})
	if p := b.problems(); len(p) != 0 {
		t.Fatalf("healthy box: %v", p)
	}
	f.failed[unit("a")] = true
	b.keys.lastOK = time.Now().Add(-48 * time.Hour)
	os.WriteFile(b.backupConf, nil, 0o600) // backups on, never ran
	p := strings.Join(b.problems(), "; ")
	for _, want := range []string{"1 VM failed", "GitHub keys", "backup"} {
		if !strings.Contains(p, want) {
			t.Errorf("problems %q lack %q", p, want)
		}
	}
	os.WriteFile(b.backupLast, []byte(fmt.Sprint(time.Now().Unix())), 0o644)
	if strings.Contains(strings.Join(b.problems(), ";"), "backup") {
		t.Error("a fresh backup still reported")
	}
}
