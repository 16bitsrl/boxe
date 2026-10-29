package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Hypervisor creates, starts and destroys microVMs on this exe-host.
type Hypervisor struct {
	BaseSnapshot string // e.g. exe/images/boxuntu-20261028@base
	Dataset      string // parent of the VM disks, e.g. exe/vms
	Kernel       string
	HostPubKey   string       // exe-host's own key, added to every VM
	HostKeyPath  string       // its private half, used to sync a VM before a copy
	ConfigDir    string       // config disks, one per VM
	HostCA       gossh.Signer // signs the VMs' host keys: clients trust <vm>.box with one line
	VMUser       string       // the hypervisors run as this user; empty: root (tests)
	VMUserID     int          // its uid and gid: disks and config are chowned to them
	VMGroupID    int
	MinFree      int64    // bytes the pool must keep free after a new VM; 0: no check
	SSHSuffix    string   // VMs are <vm>.<SSHSuffix> for SSH, e.g. box
	WebDomain    string   // and https://<vm>.<WebDomain>; empty: no web front end
	DNS          []string // resolvers for the VMs

	DevDir string                                                             // zvol devices; default /dev/zvol
	Exec   func(d time.Duration, name string, args ...string) ([]byte, error) // runs commands; default: for real
}

// errLeftBehind: a create failed and so did the cleanup; the VM's record
// stays so that `rm` can finish the job.
var errLeftBehind = errors.New("left behind")

const (
	vmUID, vmGID = 1000, 1000 // exedev in exeuntu
	runDir       = "/run/exe/vm"
)

func (h *Hypervisor) ds(name string) string { return h.Dataset + "/" + name }
func (h *Hypervisor) dev(name string) string {
	if h.DevDir != "" {
		return filepath.Join(h.DevDir, h.ds(name))
	}
	return "/dev/zvol/" + h.ds(name)
}
func (h *Hypervisor) config(name string) string {
	return filepath.Join(h.ConfigDir, name+".tar")
}
func unit(name string) string { return "exe-vm-" + name }
func sock(name string) string { return filepath.Join(runDir, name+".sock") }

// tap names come from the index: Linux limits interface names to 15 characters.
func tap(v VM) string { return fmt.Sprintf("tap%d", v.Index) }

// Every command has a deadline: a stuck one must not hold the VM lock forever.
func (h *Hypervisor) exec(d time.Duration, name string, args ...string) ([]byte, error) {
	if h.Exec != nil {
		return h.Exec(d, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (h *Hypervisor) run(name string, args ...string) error {
	_, err := h.exec(2*time.Minute, name, args...)
	return err
}

// Create clones the disk from src (a snapshot) and boots the VM. On failure
// it cleans up; if that fails too the error wraps errLeftBehind.
func (h *Hypervisor) Create(v VM, src, authorizedKeys string) error {
	err := h.create(v, src, authorizedKeys)
	if err == nil {
		return nil
	}
	if derr := h.Destroy(v); derr != nil {
		return fmt.Errorf("%w: %v (cleaning up: %v)", errLeftBehind, err, derr)
	}
	return err
}

func (h *Hypervisor) create(v VM, src, authorizedKeys string) error {
	if err := h.run("zfs", "clone", src, h.ds(v.Name)); err != nil {
		return err
	}
	if err := h.writeConfig(v, authorizedKeys); err != nil {
		return err
	}
	// The zvol device appears a moment after the clone.
	dev := h.dev(v.Name)
	for i := 0; ; i++ {
		if _, err := os.Stat(dev); err == nil {
			break
		}
		if i > 1000 {
			return fmt.Errorf("%s did not appear", dev)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h.Start(v)
}

// Copy snapshots the disk of src (running or not) and creates v from it.
// The copy is crash-consistent: like the disk after a power cut, with what
// the VM had written when it was taken.
func (h *Hypervisor) Copy(src, v VM, authorizedKeys string) error {
	// The snapshot only sees what the VM has written to disk: flush its cache.
	if h.Running(src.Name) {
		if _, err := h.exec(10*time.Second, "ssh", "-i", h.HostKeyPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"exedev@"+src.IP(), "sudo sync"); err != nil {
			return fmt.Errorf("could not flush the disk of %s: %v", src.Name, err)
		}
	}
	snap := fmt.Sprintf("%s@cp-%s-%d", h.ds(src.Name), v.Name, time.Now().Unix())
	if err := h.run("zfs", "snapshot", snap); err != nil {
		return err
	}
	err := h.Create(v, snap, authorizedKeys)
	if err != nil && !errors.Is(err, errLeftBehind) {
		h.run("zfs", "destroy", snap)
	}
	return err
}

// FreeSpace is what the VM dataset's pool has left, in bytes.
func (h *Hypervisor) FreeSpace() (int64, error) {
	out, err := h.exec(10*time.Second, "zfs", "list", "-Hp", "-o", "avail", h.Dataset)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// writeConfig writes what makes the disk this VM (hostname, DNS, SSH keys)
// to a small archive that Start attaches as a second, read-only disk;
// box-init applies it inside the VM before systemd starts. exe-host never
// mounts a VM's disk: a copied one is written by the VM, so only the VM
// reads it.
func (h *Hypervisor) writeConfig(v VM, authorizedKeys string) error {
	data, err := h.configArchive(v, authorizedKeys)
	if err != nil {
		return err
	}
	// Hypervisors (VMUser) may open their own archive, not list the others.
	if err := os.MkdirAll(h.ConfigDir, 0o711); err != nil {
		return err
	}
	if err := os.Chmod(h.ConfigDir, 0o711); err != nil {
		return err
	}
	return os.WriteFile(h.config(v.Name), data, 0o600)
}

func (h *Hypervisor) configArchive(v VM, authorizedKeys string) ([]byte, error) {
	hostKey, hostPub, hostCert, err := h.newHostKey(v.Name)
	if err != nil {
		return nil, err
	}
	// The id tells box-init whether the archive is already applied. A new
	// one on every write: a copy carries its source's id.
	id := make([]byte, 16)
	machineID := make([]byte, 16)
	rand.Read(id)
	rand.Read(machineID)
	files := []struct {
		path, data string
		mode       int64
		user       bool
	}{
		{"etc/hostname", v.Name + "\n", 0o644, false},
		{"etc/hosts", fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n", v.Name), 0o644, false},
		{"etc/resolv.conf", resolvConf(h.DNS), 0o644, false},
		{"etc/machine-id", hex.EncodeToString(machineID) + "\n", 0o444, false},
		{"etc/ssh/ssh_host_ed25519_key", hostKey, 0o600, false},
		{"etc/ssh/ssh_host_ed25519_key.pub", hostPub, 0o644, false},
		{"etc/ssh/ssh_host_ed25519_key-cert.pub", hostCert, 0o644, false},
		{"home/exedev/.ssh/authorized_keys", authorizedKeys + h.HostPubKey, 0o600, true},
		{"etc/box/web-domain", h.WebDomain + "\n", 0o644, false},
		{"home/exedev/.config/box/AGENTS.md", h.agentsMD(v), 0o644, true},
		{"etc/box/id", hex.EncodeToString(id) + "\n", 0o644, false},
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, d := range []struct {
		name string
		mode int64
	}{{"home/exedev/.ssh", 0o700}, {"home/exedev/.config/box", 0o755}} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d.name, Mode: d.mode,
			Uid: vmUID, Gid: vmGID, ModTime: now}); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.path, Mode: f.mode, Size: int64(len(f.data)), ModTime: now}
		if f.user {
			hdr.Uid, hdr.Gid = vmUID, vmGID
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func resolvConf(servers []string) string {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	return b.String()
}

// agentsMD tells the coding agents in the VM (Claude, Codex, opencode,
// Shelley: the image links their instruction files here) where they are.
func (h *Hypervisor) agentsMD(v VM) string {
	web := "- There is no web front end: reach servers in the VM with `ssh -L`."
	if h.WebDomain != "" {
		web = fmt.Sprintf("- Web: an HTTP server on port 8000 is published at https://%[1]s.%[2]s, ports 3000-9999 at https://%[1]s--<port>.%[2]s (e.g. https://%[1]s--3000.%[2]s). HTTPS ends at the proxy: serve plain HTTP and listen on 0.0.0.0, not 127.0.0.1.\n"+
			"- Only logged-in members of the team reach these URLs. The proxy sends the user's email in the `X-Box-Email` header (it drops any `X-Box-*` header sent by clients) and `X-Forwarded-Proto: https`.", v.Name, h.WebDomain)
	}
	return fmt.Sprintf("You are running in a box VM named %s: a microVM on the team's own infrastructure. It is similar to exe.dev, but exe.dev's services (Shelley, LLM gateway, integrations, `ssh exe.dev`) are not available here.\n\n"+
		"- Ubuntu, user `exedev` with passwordless sudo, persistent disk. Docker works.\n"+
		"- Internet works. The team's LAN, the host and the other VMs are not reachable from here.\n"+
		"%s\n"+
		"- VMs are created, copied and deleted from the user's computer with `ssh box` (`ssh box help`), not from inside the VM.\n", v.Name, web)
}

// newHostKey makes the VM's SSH host key and its certificate, signed by
// HostCA for the name users type, <vm>.box.
func (h *Hypervisor) newHostKey(name string) (priv, pub, cert string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	block, err := gossh.MarshalPrivateKey(sk, "root@"+name)
	if err != nil {
		return "", "", "", err
	}
	sshPub, err := gossh.NewPublicKey(pk)
	if err != nil {
		return "", "", "", err
	}
	c := &gossh.Certificate{
		Key:             sshPub,
		CertType:        gossh.HostCert,
		KeyId:           name,
		ValidPrincipals: []string{name + "." + h.SSHSuffix},
		ValidBefore:     gossh.CertTimeInfinity,
	}
	if err := c.SignCert(rand.Reader, h.HostCA); err != nil {
		return "", "", "", err
	}
	return string(pem.EncodeToMemory(block)), string(gossh.MarshalAuthorizedKey(sshPub)), string(gossh.MarshalAuthorizedKey(c)), nil
}

// Start boots v if it is not running. Idempotent: also used at daemon start
// to bring VMs back after an exe-host reboot.
func (h *Hypervisor) Start(v VM) error {
	if h.Running(v.Name) {
		return h.guard(v) // net-setup.sh may have run again since
	}
	h.run("systemctl", "reset-failed", unit(v.Name)) // a failed unit keeps its name
	mib, err := memMiB(v.Mem)
	if err != nil {
		return err
	}
	// A fresh tap, owned by the user the hypervisor runs as.
	t := tap(v)
	if _, err := net.InterfaceByName(t); err == nil {
		h.run("ip", "link", "del", t)
	}
	add := []string{"tuntap", "add", t, "mode", "tap"}
	if h.VMUser != "" {
		add = append(add, "user", h.VMUser)
	}
	if err := h.run("ip", add...); err != nil {
		return err
	}
	if err := h.guard(v); err != nil {
		return err
	}
	steps := [][]string{
		{"ip", "link", "set", t, "master", "br0", "up"},
		// Isolated bridge ports cannot talk to each other: VMs do not see each other.
		{"bridge", "link", "set", "dev", t, "isolated", "on"},
		// Known MAC: exe-host does not wait for ARP, which retries only once a second.
		{"ip", "neigh", "replace", v.IP(), "lladdr", v.MAC(), "dev", "br0", "nud", "permanent"},
	}
	for _, s := range steps {
		if err := h.run(s[0], s[1:]...); err != nil {
			return err
		}
	}
	disk, err := filepath.EvalSymlinks(h.dev(v.Name))
	if err != nil {
		return err
	}
	os.MkdirAll(runDir, 0o755)
	os.Remove(sock(v.Name))
	// The hypervisor is the part a VM could break out to: it runs as an
	// unprivileged user, sees only its own disks, and cannot use more memory
	// and CPU than the VM was given (plus room for itself).
	props := []string{
		"Restart=on-failure", "RestartSec=2",
		fmt.Sprintf("MemoryMax=%dM", mib+mib/8+256),
		fmt.Sprintf("CPUQuota=%d%%", v.CPUs*100),
		"TasksMax=1024",
		"NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=yes", "PrivateTmp=yes",
		"ProtectKernelModules=yes", "ProtectKernelLogs=yes", "ProtectControlGroups=yes",
		"CapabilityBoundingSet=", "ReadWritePaths=" + runDir,
		"DevicePolicy=closed", "DeviceAllow=/dev/kvm rw", "DeviceAllow=/dev/net/tun rw", "DeviceAllow=" + disk + " rw",
	}
	if h.VMUserID > 0 {
		for _, p := range []string{disk, h.config(v.Name)} {
			if err := os.Chown(p, h.VMUserID, h.VMGroupID); err != nil {
				return err
			}
		}
		os.Chown(runDir, h.VMUserID, h.VMGroupID)
	}
	if h.VMUser != "" {
		props = append(props, "User="+h.VMUser, "Group="+h.VMUser, "SupplementaryGroups=kvm")
	}
	args := []string{"-q", "--unit", unit(v.Name)}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	cmdline := fmt.Sprintf("console=ttyS0 root=/dev/vda rw init=/usr/local/bin/box-init ip=%s::10.42.0.1:255.255.0.0:%s:eth0:off quiet", v.IP(), v.Name)
	args = append(args, "/usr/local/bin/cloud-hypervisor",
		"--api-socket", sock(v.Name),
		"--kernel", h.Kernel,
		"--cmdline", cmdline,
		"--disk", "path="+disk+",image_type=raw",
		"path="+h.config(v.Name)+",image_type=raw,readonly=on",
		"--cpus", fmt.Sprintf("boot=%d", v.CPUs),
		"--memory", "size="+v.Mem,
		"--net", "tap="+t+",mac="+v.MAC(),
		// The console goes to the journal (journalctl -u exe-vm-<name>),
		// which caps its size, instead of a file that grows forever.
		"--serial", "tty",
		"--console", "off")
	return h.run("systemd-run", args...)
}

// Stop powers v off (a power cut: the VM is not asked to shut down).
func (h *Hypervisor) Stop(v VM) error {
	h.run("systemctl", "stop", unit(v.Name))
	if h.Running(v.Name) {
		return fmt.Errorf("%s is still running", v.Name)
	}
	h.run("systemctl", "reset-failed", unit(v.Name))
	return nil
}

// guard allows the VM's tap to send with the VM's own MAC and IP only (the
// rules are in net-setup.sh). Done before the tap is up; idempotent.
func (h *Hypervisor) guard(v VM) error {
	if err := h.run("nft", fmt.Sprintf(`add element bridge box mac { "%s" . %s }`, tap(v), v.MAC())); err != nil {
		return err
	}
	return h.run("nft", fmt.Sprintf(`add element bridge box ip4 { "%s" . %s }`, tap(v), v.IP()))
}

// State is running, starting, failed or stopped.
func (h *Hypervisor) State(name string) string {
	out, _ := h.exec(5*time.Second, "systemctl", "is-active", unit(name))
	switch strings.TrimSpace(string(out)) {
	case "active", "reloading":
		return "running"
	case "activating":
		return "starting"
	case "failed":
		return "failed"
	}
	return "stopped"
}

func (h *Hypervisor) Running(name string) bool {
	s := h.State(name)
	return s == "running" || s == "starting"
}

// WaitSSH waits until sshd in the VM sends its banner, or the VM stops.
func (h *Hypervisor) WaitSSH(v VM, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for i := 0; time.Now().Before(deadline); i++ {
		if i%50 == 49 && !h.Running(v.Name) {
			return fmt.Errorf("%s stopped while booting (journalctl -u %s on exe-host)", v.Name, unit(v.Name))
		}
		c, err := net.DialTimeout("tcp", v.IP()+":22", 300*time.Millisecond)
		if err == nil {
			c.SetReadDeadline(time.Now().Add(time.Second))
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if strings.HasPrefix(line, "SSH-") {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("%s: no SSH after %s", v.Name, timeout)
}

// Destroy stops v and deletes its disk. Copies made from v keep working:
// they are promoted so that they no longer depend on v's snapshots.
// Idempotent; the config disk and network entries go only once the disk is
// gone, so a failed Destroy leaves a VM that can still start or be retried.
func (h *Hypervisor) Destroy(v VM) error {
	if err := h.Stop(v); err != nil {
		return err
	}
	if h.diskExists(v.Name) {
		if err := h.run("zfs", "destroy", "-r", h.ds(v.Name)); err != nil {
			out, err := h.exec(time.Minute, "zfs", "list", "-H", "-t", "snapshot", "-o", "clones", "-r", h.ds(v.Name))
			if err != nil {
				return err
			}
			for _, clone := range strings.Fields(strings.ReplaceAll(string(out), ",", " ")) {
				// Only copies made by other VMs: anything else (a backup
				// reading the disk right now) must finish first.
				if clone != "-" && !strings.HasPrefix(clone, h.Dataset+"/") {
					return fmt.Errorf("%s is in use by %s, try again later", v.Name, clone)
				}
				if clone != "-" {
					if err := h.run("zfs", "promote", clone); err != nil {
						return err
					}
				}
			}
			if err := h.run("zfs", "destroy", "-r", h.ds(v.Name)); err != nil {
				return err
			}
		}
	}
	// What is left may already be gone: errors do not matter.
	h.run("ip", "link", "del", tap(v))
	h.run("nft", fmt.Sprintf(`delete element bridge box mac { "%s" . %s }`, tap(v), v.MAC()))
	h.run("nft", fmt.Sprintf(`delete element bridge box ip4 { "%s" . %s }`, tap(v), v.IP()))
	h.run("ip", "neigh", "del", v.IP(), "dev", "br0")
	os.Remove(sock(v.Name))
	os.Remove(h.config(v.Name))
	return nil
}

func (h *Hypervisor) diskExists(name string) bool {
	out, err := h.exec(30*time.Second, "zfs", "list", "-H", "-o", "name", h.ds(name))
	return err == nil || !strings.Contains(string(out), "does not exist")
}

// memMiB parses sizes like 512M and 4G.
func memMiB(s string) (int, error) {
	if len(s) < 2 || (s[len(s)-1] != 'M' && s[len(s)-1] != 'G') {
		return 0, fmt.Errorf("size like 512M or 4G: %q", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 || n > 1<<20 {
		return 0, fmt.Errorf("size like 512M or 4G: %q", s)
	}
	if s[len(s)-1] == 'G' {
		n *= 1024
	}
	return n, nil
}
