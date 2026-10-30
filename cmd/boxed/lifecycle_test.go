package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHost stands in for the commands boxed runs on the host (zfs, systemctl,
// ip, nft, ssh): it records them and fails the ones it is told to.
type fakeHost struct {
	mu        sync.Mutex
	cmds      []string
	fail      map[string]bool  // command prefix -> fails
	running   map[string]bool  // unit -> active
	failed    map[string]bool  // unit -> failed
	paused    map[string]bool  // unit -> hypervisor restored, VM not resumed yet
	cpu       map[string]int64 // unit -> CPU time used, in ns
	devDir    string
	agent     []string                         // commands boxe-agent got, as "<vm> <command>"
	noAgent   bool                             // VMs from an image without boxe-agent
	agentErr  map[string]string                // command -> error boxe-agent answers
	hangUp    map[string]int                   // "vm command" -> times boxe-agent hangs up without answering
	schedule  map[string]string                // vm -> what boxe-agent answers to schedule, after ok
	noShelley bool                             // Shelley in the VM does not answer
	noModels  bool                             // nobody added a model to Shelley yet
	origin    map[string]string                // clone -> snapshot it was made from
	images    []string                         // snapshots under the images dataset
	mem       map[string]int64                 // unit -> RAM it takes, in bytes
	disk      string                           // zfs list of the VM disks: name, referenced
	onSSH     func()                           // runs once, when something next waits for a VM's sshd
	journal   map[string]string                // unit -> what its process printed, as journalctl shows it
	inVM      func(cmd string) ([]byte, error) // runs the other commands sent to a VM over SSH
}

// dial connects to a VM: every sshd answers at once, and boxe-agent behind
// the vsock socket answers as the real one does.
func (f *fakeHost) dial(network, addr string, _ time.Duration) (net.Conn, error) {
	c, s := net.Pipe()
	if network != "unix" {
		f.mu.Lock()
		hook := f.onSSH
		f.onSSH = nil
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		go s.Write([]byte("SSH-2.0-fake\r\n"))
		return c, nil
	}
	if f.noAgent {
		return nil, errors.New("connection refused")
	}
	vm := filepath.Base(filepath.Dir(addr))
	go func() {
		defer s.Close()
		r := bufio.NewReader(s)
		if line, _ := r.ReadString('\n'); line != "CONNECT 1024\n" {
			return
		}
		s.Write([]byte("OK 1073741825\n"))
		cmd, _ := r.ReadString('\n')
		cmd = strings.TrimSpace(cmd)
		f.mu.Lock()
		f.agent = append(f.agent, vm+" "+cmd)
		if f.hangUp[vm+" "+cmd] > 0 {
			f.hangUp[vm+" "+cmd]--
			f.mu.Unlock()
			return
		}
		reply := "ok"
		if cmd == "schedule" {
			reply += "\n" + strings.TrimSuffix(f.schedule[vm], "\n")
		}
		if e, ok := f.agentErr[cmd]; ok {
			reply = "error: " + e
		}
		f.mu.Unlock()
		s.Write([]byte(reply + "\n"))
	}()
	return c, nil
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
	case name == "journalctl":
		for _, a := range args {
			if u, ok := strings.CutPrefix(a, "_SYSTEMD_UNIT="); ok {
				return []byte(f.journal[strings.TrimSuffix(u, ".service")]), nil
			}
		}
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
				if f.running[args[i+1]] {
					return []byte("Unit " + args[i+1] + ".service was already loaded"), errors.New("exit status 1")
				}
				f.running[args[i+1]] = true
				f.paused[args[i+1]] = strings.Contains(cmd, "resume=false")
			}
		}
	case strings.HasPrefix(cmd, "zfs clone"):
		ds := args[len(args)-1]
		f.origin[ds] = args[len(args)-2]
		os.MkdirAll(filepath.Dir(filepath.Join(f.devDir, ds)), 0o755)
		os.WriteFile(filepath.Join(f.devDir, ds), nil, 0o600)
	case strings.HasPrefix(cmd, "zfs list -H -o name -t all -r"):
		ds := args[len(args)-1]
		all := []string{ds}
		for _, snap := range f.images {
			if strings.HasPrefix(snap, ds+"@") || strings.HasPrefix(snap, ds+"/") {
				all = append(all, snap)
			}
		}
		return []byte(strings.Join(all, "\n") + "\n"), nil
	case strings.HasPrefix(cmd, "zfs list -H -o name"):
		if _, err := os.Stat(filepath.Join(f.devDir, args[len(args)-1])); err != nil {
			return []byte("dataset does not exist"), errors.New("exit status 1")
		}
	case strings.HasPrefix(cmd, "zfs list -t snapshot"):
		return []byte(strings.Join(f.images, "\n") + "\n"), nil
	case strings.HasPrefix(cmd, "zfs list -Hp -o name,referenced"):
		return []byte(f.disk), nil
	case strings.HasPrefix(cmd, "zfs list -H -o clones"):
		var clones []string
		for ds, snap := range f.origin {
			if _, err := os.Stat(filepath.Join(f.devDir, ds)); err == nil && snap == args[len(args)-1] {
				clones = append(clones, ds)
			}
		}
		if len(clones) == 0 {
			return []byte("-\n"), nil
		}
		return []byte(strings.Join(clones, ",") + "\n"), nil
	case strings.HasPrefix(cmd, "zfs list"):
		return []byte("-\n"), nil
	case strings.HasPrefix(cmd, "zfs destroy -r"):
		os.Remove(filepath.Join(f.devDir, args[len(args)-1]))
	case strings.HasPrefix(cmd, "zfs destroy"):
		target := args[len(args)-1]
		kept := f.images[:0]
		for _, snap := range f.images {
			if snap != target && !strings.HasPrefix(snap, target+"@") {
				kept = append(kept, snap)
			}
		}
		f.images = kept
	case strings.HasPrefix(cmd, "zfs get -H -o value origin"):
		if o := f.origin[args[len(args)-1]]; o != "" {
			return []byte(o + "\n"), nil
		}
		return []byte("-\n"), nil
	case strings.HasPrefix(cmd, "systemctl show -p MemoryCurrent"):
		if n, ok := f.mem[args[len(args)-1]]; ok {
			return []byte(fmt.Sprintln(n)), nil
		}
		return []byte("[not set]\n"), nil
	case strings.HasPrefix(cmd, "systemctl show -p CPUUsageNSec"):
		return []byte(fmt.Sprintln(f.cpu[args[len(args)-1]])), nil
	case name == "ssh" && strings.Contains(cmd, "shelley client chat"):
		if f.noShelley {
			return []byte("dial unix: connection refused"), errors.New("exit status 1")
		}
		if f.noModels {
			return []byte("no models\n"), errors.New("exit status 3")
		}
		return []byte(`{"conversation_id":"cid1","slug":"fix-the-bug"}` + "\n"), nil
	case name == "ssh" && f.inVM != nil:
		return f.inVM(args[len(args)-1])
	case name == "ch-remote":
		// ch-remote --api-socket <sock> <command> [url]
		u := unit(filepath.Base(filepath.Dir(args[1])))
		if !f.running[u] {
			return []byte("cannot connect"), errors.New("exit status 1")
		}
		switch args[2] {
		case "snapshot":
			dir := strings.TrimPrefix(args[3], "file://")
			os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600)
			os.WriteFile(filepath.Join(dir, "memory-ranges"), nil, 0o600)
			os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"cpus":{"boot_vcpus":2},`+
				`"disks":[{"id":"_disk0","path":"/dev/zd0"},{"id":"_disk1","path":"/old/config.tar","readonly":true}],`+
				`"net":[{"id":"_net2","tap":"tap99","mac":"02:42:0a:2a:00:63"}],"vsock":{"cid":3,"socket":"/old.vsock"}}`), 0o600)
		case "shutdown-vmm":
			delete(f.running, u)
		case "info":
			if f.paused[u] {
				return []byte(`{"state":"Paused"}`), nil
			}
			return []byte(`{"state":"Running"}`), nil
		case "resume":
			delete(f.paused, u)
		}
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
	f := &fakeHost{fail: map[string]bool{}, running: map[string]bool{}, failed: map[string]bool{}, paused: map[string]bool{}, cpu: map[string]int64{},
		agentErr: map[string]string{}, schedule: map[string]string{}, hangUp: map[string]int{}, origin: map[string]string{}, devDir: t.TempDir()}
	h := &Hypervisor{BaseSnapshot: "boxe/images/base@base", Dataset: "boxe/vms", Kernel: "vmlinux",
		ConfigDir: t.TempDir(), SleepDir: t.TempDir(), StateDir: t.TempDir(), HostCA: ca, DevDir: f.devDir, Exec: f.exec, Dial: f.dial}
	return h, f
}

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
	h.VMUser = "boxe-vm"
	h.Boxed = "/usr/local/sbin/boxed"
	h.StateDir = "/var/lib/boxe"
	v := VM{Name: "test", Index: 2, CPUs: 2, Mem: "2G"}
	h.writeConfig(v, "")
	os.MkdirAll(filepath.Join(f.devDir, "boxe/vms"), 0o755)
	os.WriteFile(filepath.Join(f.devDir, h.ds(v.Name)), nil, 0o600)
	h.Start(v)
	var run string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "systemd-run") {
			run = c
		}
	}
	for _, want := range []string{"-p User=boxe-vm", "-p NoNewPrivileges=yes", "-p ProtectSystem=strict",
		"-p MemoryMax=", "-p CPUQuota=200%", "-p Restart=on-failure", "--serial tty", "-p CapabilityBoundingSet=",
		// Stopping the unit (restart, rm, host shutdown) asks the VM to shut down first.
		"-p ExecStop=/usr/local/sbin/boxed -power-off " + sock(v.Name), "-p TimeoutStopSec="} {
		if !strings.Contains(run, want) {
			t.Errorf("systemd-run lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "/var/log/boxe") {
		t.Error("console still goes to an unbounded file")
	}
	// It sees its own files and none of the other VMs': their config disks
	// (with their SSH keys), control sockets and saved memory, nor the
	// database.
	for _, want := range []string{"-p TemporaryFileSystem=/var/lib/boxe:ro",
		"-p BindReadOnlyPaths=vmlinux", "-p BindReadOnlyPaths=" + h.config(v.Name),
		"-p TemporaryFileSystem=" + runDir + ":ro", "-p BindPaths=" + vmRunDir(v.Name),
		"-p TemporaryFileSystem=" + h.SleepDir + ":ro", "-p BindPaths=" + h.sleepDir(v.Name),
		"-p InaccessiblePaths=-/etc/boxe"} {
		if !strings.Contains(run, want) {
			t.Errorf("systemd-run lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "ReadWritePaths="+runDir+" ") {
		t.Error("the hypervisor can write every VM's sockets")
	}
	if filepath.Dir(sock(v.Name)) != vmRunDir(v.Name) || filepath.Dir(vsockPath(v.Name)) != vmRunDir(v.Name) {
		t.Errorf("sockets outside the VM's own directory: %s %s", sock(v.Name), vsockPath(v.Name))
	}
	if !f.ran("ip tuntap add tap2 mode tap user boxe-vm") {
		t.Error("tap not owned by the VM user")
	}
}

func TestAdmission(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newFakeHypervisor(t)
	b := &Box{store: s, hv: h, limits: Limits{VMMem: 8 << 10, UserMem: 6 << 10, UserVMs: 3}}
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
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
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
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b := &Box{store: s, hv: h, keys: &Keys{store: s}, backupConf: filepath.Join(dir, "backup.conf"), backupLast: filepath.Join(dir, "backup-last")}
	b.keys.lastOK = time.Now()
	s.Insert(&VM{Name: "a", Owner: "u", CPUs: 1, Mem: "1G"})
	if p := b.problems(); len(p) != 0 {
		t.Fatalf("healthy boxe: %v", p)
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

// fakeVM is a hypervisor process and its API socket: the power button ends
// the process, as a guest that shuts down when asked.
func fakeVM(t *testing.T, obeys bool) (sock string, pid int, pressed *atomic.Bool) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { cmd.Process.Kill() })
	sock = filepath.Join(t.TempDir(), "vm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pressed = &atomic.Bool{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Path == "/api/v1/vm.power-button" {
			pressed.Store(true)
			if obeys {
				go func() { time.Sleep(100 * time.Millisecond); cmd.Process.Kill() }()
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, cmd.Process.Pid, pressed
}

func TestPowerOffWaitsForTheGuest(t *testing.T) {
	sock, pid, pressed := fakeVM(t, true)
	start := time.Now()
	if err := powerOff(sock, pid, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !pressed.Load() {
		t.Error("power button not pressed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v for a guest that shuts down at once", d)
	}
}

func TestPowerOffGivesUp(t *testing.T) {
	sock, pid, _ := fakeVM(t, false)
	start := time.Now()
	if err := powerOff(sock, pid, 500*time.Millisecond); err == nil {
		t.Error("no error although the guest ignored the button")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v, want about the timeout", d)
	}
}

// Deleting a copy also deletes the snapshot of its source it was made from,
// which would otherwise keep old data of the source for ever.
func TestDestroyACopyDeletesItsSnapshot(t *testing.T) {
	h, f := newFakeHypervisor(t)
	src := VM{Name: "src", Index: 2, CPUs: 1, Mem: "1G"}
	if err := h.Create(src, h.BaseSnapshot, ""); err != nil {
		t.Fatal(err)
	}
	cp := VM{Name: "cp", Index: 3, CPUs: 1, Mem: "1G"}
	if err := h.Copy(src, cp, ""); err != nil {
		t.Fatal(err)
	}
	snap := f.origin[h.ds("cp")]
	if err := h.Destroy(cp); err != nil {
		t.Fatal(err)
	}
	if !f.ran("zfs destroy " + snap) {
		t.Errorf("%s left behind", snap)
	}
	// The base image's snapshot is not the VM's to delete.
	f.cmds = nil
	h.Destroy(src)
	if f.ran("zfs destroy " + h.BaseSnapshot) {
		t.Error("deleted the base image")
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boxe-run")
	if err != nil {
		panic(err)
	}
	runDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
