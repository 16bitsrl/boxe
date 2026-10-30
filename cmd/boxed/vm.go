package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Hypervisor creates, starts and destroys microVMs on this host.
type Hypervisor struct {
	BaseSnapshot string // e.g. boxe/images/boxeuntu-20261028@base
	Dataset      string // parent of the VM disks, e.g. boxe/vms
	Kernel       string
	HostPubKey   string       // the host's own key, added to every VM
	HostKeyPath  string       // its private half, used to sync a VM before a copy
	StateDir     string       // hidden from the hypervisors but for their own kernel and config disk
	ConfigDir    string       // config disks, one per VM
	SleepDir     string       // memory of sleeping VMs, one directory per VM; empty: VMs never sleep
	HostCA       gossh.Signer // signs the VMs' host keys: clients trust <vm>.boxe with one line
	VMUser       string       // the hypervisors run as this user; empty: root (tests)
	VMUserID     int          // its uid and gid: disks and config are chowned to them
	VMGroupID    int
	MinFree      int64  // bytes the pool must keep free after a new VM; 0: no check
	SSHSuffix    string // VMs are <vm>.<SSHSuffix> for SSH, e.g. boxe
	WebDomain    string // and https://<vm>.<WebDomain>; empty: no web front end
	Boxed        string // this program, run by systemd to shut a VM down; empty: no clean shutdown
	LLMGateway   string // the LLM gateway as VMs reach it, e.g. http://10.42.0.1:2998; empty: none

	DevDir string                                                              // zvol devices; default /dev/zvol
	Exec   func(d time.Duration, name string, args ...string) ([]byte, error)  // runs commands; default: for real
	Dial   func(network, addr string, timeout time.Duration) (net.Conn, error) // connects to VMs; default: net.DialTimeout
}

// errLeftBehind: a create failed and so did the cleanup; the VM's record
// stays so that `rm` can finish the job.
var errLeftBehind = errors.New("left behind")

const vmUID, vmGID = 1000, 1000 // exedev in exeuntu

var runDir = "/run/boxe/vm" // a directory per VM, see vmRunDir

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
func unit(name string) string { return "boxe-vm-" + name }

// checkSetup says what the host lacks to run VMs, so that boxed stops at
// start instead of failing every new VM with a vaguer error.
func (h *Hypervisor) checkSetup(lookPath func(string) (string, error)) error {
	if _, err := os.Stat(h.Kernel); err != nil {
		return fmt.Errorf("-kernel: %w", err)
	}
	if _, err := lookPath("/usr/local/bin/cloud-hypervisor"); err != nil {
		return fmt.Errorf("cloud-hypervisor: %w", err)
	}
	// Sleep and fork drive the hypervisor through its API.
	if _, err := lookPath("ch-remote"); h.SleepDir != "" && err != nil {
		return fmt.Errorf("-sleep-dir needs ch-remote, from the Cloud Hypervisor release: %w", err)
	}
	return nil
}

// vmRunDir holds a VM's control socket and vsock: its hypervisor sees only
// its own.
func vmRunDir(name string) string  { return filepath.Join(runDir, name) }
func sock(name string) string      { return filepath.Join(vmRunDir(name), "api.sock") }
func vsockPath(name string) string { return filepath.Join(vmRunDir(name), "vsock") }

// tap names come from the index: Linux limits interface names to 15 characters.
func tap(v VM) string { return fmt.Sprintf("tap%d", v.Index) }

// Every command has a deadline: a stuck one must not hold the VM lock forever.
func (h *Hypervisor) exec(d time.Duration, name string, args ...string) ([]byte, error) {
	if h.Exec != nil {
		return h.Exec(d, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var out capped
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = &out, &out
	c.WaitDelay = time.Second // what it started may keep its output open
	if err := c.Run(); err != nil {
		return out.b, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out.b)))
	}
	return out.b, nil
}

// maxOutput is the most of a command's output kept: a VM's command (exec)
// may print without end.
const maxOutput = 16 << 20

// capped keeps the first maxOutput bytes written to it.
type capped struct{ b []byte }

func (c *capped) Write(p []byte) (int, error) {
	c.b = append(c.b, p[:min(len(p), maxOutput-len(c.b))]...)
	return len(p), nil
}

func (h *Hypervisor) dial(network, addr string, timeout time.Duration) (net.Conn, error) {
	if h.Dial != nil {
		return h.Dial(network, addr, timeout)
	}
	return net.DialTimeout(network, addr, timeout)
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
	if err := h.waitDev(v); err != nil {
		return err
	}
	return h.Start(v)
}

// waitDev waits for v's zvol device, which appears a moment after the clone.
func (h *Hypervisor) waitDev(v VM) error {
	dev := h.dev(v.Name)
	for i := 0; ; i++ {
		if _, err := os.Stat(dev); err == nil {
			return nil
		}
		if i > 1000 {
			return fmt.Errorf("%s did not appear", dev)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Copy snapshots the disk of src (running or not) and creates v from it.
// The copy is crash-consistent: like the disk after a power cut, with what
// the VM had written when it was taken.
func (h *Hypervisor) Copy(src, v VM, authorizedKeys string) error {
	// The snapshot only sees what the VM has written to disk: flush its cache.
	if h.Running(src.Name) {
		if err := h.flush(src); err != nil {
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

// flush has v write its cached data to its disk.
func (h *Hypervisor) flush(v VM) error {
	_, err := h.inVM(v, 10*time.Second, "sudo sync")
	return err
}

// inVM runs cmd in v as exedev, over SSH with the host's own key. Only if
// the VM at v's address shows a certificate for v's name: another VM that
// took the address meanwhile is not sent cmd.
func (h *Hypervisor) inVM(v VM, timeout time.Duration, cmd string) ([]byte, error) {
	known := filepath.Join(h.StateDir, "vm_known_hosts")
	line := "@cert-authority *." + h.SSHSuffix + " " + string(gossh.MarshalAuthorizedKey(h.HostCA.PublicKey()))
	if b, err := os.ReadFile(known); err != nil || string(b) != line {
		if err := os.WriteFile(known, []byte(line), 0o644); err != nil {
			return nil, err
		}
	}
	return h.exec(timeout, "ssh", "-i", h.HostKeyPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "-o", "HostKeyAlias="+v.Name+"."+h.SSHSuffix,
		"-o", "LogLevel=ERROR", "exedev@"+v.IP(), cmd)
}

// StartAgent starts a Shelley conversation in v on prompt and returns its
// slug, for https://<vm>--agent.<domain>/c/<slug>. Shelley uses the models
// and keys its owner set up in it: boxe has no credentials of its own.
func (h *Hypervisor) StartAgent(v VM, prompt string) (string, error) {
	quoted := "'" + strings.ReplaceAll(prompt, "'", `'\''`) + "'"
	// Without a model of its own Shelley has only a fake one, for tests,
	// which it does not list.
	out, err := h.inVM(v, 30*time.Second, `m=$(curl -sf --unix-socket ~/.config/shelley/shelley.sock http://shelley/api/models) || exit 1; `+
		`[ "$m" != "[]" ] || { echo no models; exit 3; }; shelley client chat -cwd /home/exedev -p `+quoted)
	if err != nil && strings.TrimSpace(string(out)) == "no models" {
		return "", fmt.Errorf("its agent has no model yet: add your API key at https://%s--agent.%s, or copy (cp or fork) a VM where you did", v.Name, h.WebDomain)
	}
	if err != nil {
		return "", fmt.Errorf("its agent did not start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var r struct {
		ID   string `json:"conversation_id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.ID == "" {
		return "", fmt.Errorf("unexpected answer from Shelley: %q", out)
	}
	if r.Slug == "" {
		return r.ID, nil
	}
	return r.Slug, nil
}

// Console is the last lines v printed on its serial console (see start),
// since it was made: the journal keeps those of an older VM of that name.
func (h *Hypervisor) Console(v VM, lines int) (string, error) {
	out, err := h.exec(10*time.Second, "journalctl", "_SYSTEMD_UNIT="+unit(v.Name)+".service",
		"--since", fmt.Sprintf("@%d", v.Created.Unix()), "-n", strconv.Itoa(lines), "-o", "cat", "-q", "--no-pager")
	return plainText(string(out)), err
}

// terminalCodes are what a terminal reads as colors and cursor moves, and
// the other control characters but tab and newline.
var terminalCodes = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]|[\x00-\x08\x0b-\x1f\x7f]`)

// plainText is what a VM printed, without the codes for a terminal.
func plainText(s string) string { return terminalCodes.ReplaceAllString(s, "") }

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
// boxe-init applies it inside the VM before systemd starts. The host never
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

// vmHostname is the VM's full name, <vm>.<domain>: Shelley shows it, and
// adds .exe.xyz to a name without dots. The kernel takes up to 64 characters.
func vmHostname(name, domain string) string {
	if domain == "" || len(name)+1+len(domain) > 64 {
		return name
	}
	return name + "." + domain
}

func (h *Hypervisor) configArchive(v VM, authorizedKeys string) ([]byte, error) {
	hostKey, hostPub, hostCert, err := h.newHostKey(v.Name)
	if err != nil {
		return nil, err
	}
	// The id tells boxe-init whether the archive is already applied. A new
	// one on every write: a copy carries its source's id.
	id := make([]byte, 16)
	machineID := make([]byte, 16)
	rand.Read(id)
	rand.Read(machineID)
	host, names := vmHostname(v.Name, h.WebDomain), v.Name
	if host != v.Name {
		names = host + " " + v.Name
	}
	type file struct {
		path, data string
		mode       int64
		user       bool
	}
	files := []file{
		{"etc/hostname", host + "\n", 0o644, false},
		{"etc/hosts", fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n", names), 0o644, false},
		{"etc/resolv.conf", "nameserver " + gatewayIP + "\n", 0o644, false},
		{"etc/machine-id", hex.EncodeToString(machineID) + "\n", 0o444, false},
		{"etc/ssh/ssh_host_ed25519_key", hostKey, 0o600, false},
		{"etc/ssh/ssh_host_ed25519_key.pub", hostPub, 0o644, false},
		{"etc/ssh/ssh_host_ed25519_key-cert.pub", hostCert, 0o644, false},
		{"home/exedev/.ssh/authorized_keys", authorizedKeys + h.HostPubKey, 0o600, true},
		{"etc/boxe/web-domain", h.WebDomain + "\n", 0o644, false},
		{"etc/boxe/net", fmt.Sprintf("ADDRESS=%s/16\nGATEWAY=10.42.0.1\nMAC=%s\n", v.IP(), v.MAC()), 0o644, false},
		{"home/exedev/.config/boxe/AGENTS.md", h.agentsMD(v), 0o644, true},
		{"etc/boxe/id", hex.EncodeToString(id) + "\n", 0o644, false},
	}
	if g := h.LLMGateway; g != "" {
		// A proxy set in the VM must not answer for the host.
		host, _, _ := net.SplitHostPort(strings.TrimPrefix(g, "http://"))
		// From the configuration, not the image: new VMs of every image,
		// and copies of older VMs, get them. VMs made before do not.
		files = append(files[:len(files)-1], []file{
			{"etc/profile.d/boxe-llm.sh", "# The API keys of the user's boxe account (ssh boxe llm): the host adds them\n" +
				"# to the requests that go through its gateway.\n" +
				"eval \"$(curl -sf --noproxy " + host + " --connect-timeout 1 --max-time 2 " + g + "/env 2>/dev/null)\"\n", 0o644, false},
			{"etc/systemd/system/shelley.service.d/llm.conf", "# Shelley uses the boxe gateway when its user has a key there.\n[Service]\n" +
				"ExecStartPre=-/bin/sh -c 'curl -sf --noproxy " + host + " --connect-timeout 1 --max-time 3 -o " + shelleyConfig + " " + g + "/shelley.json || echo {} > " + shelleyConfig + "'\n" +
				"ExecStart=\nExecStart=/usr/local/bin/shelley -db /home/exedev/.config/shelley/shelley.db -config " + shelleyConfig +
				" serve -systemd-activation -require-header X-Box-Email\n", 0o644, false},
			files[len(files)-1], // the id stays last
		}...)
	}
	if h.SleepDir != "" {
		// The welcome message tells users the VM sleeps (image/boxe-welcome.sh).
		files = append(files[:len(files)-1], file{"etc/boxe/sleeps", "", 0o644, false}, files[len(files)-1])
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, d := range []struct {
		name string
		mode int64
	}{{"home/exedev/.ssh", 0o700}, {"home/exedev/.config/boxe", 0o755}} {
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
	if buf.Len() > configSize {
		return nil, fmt.Errorf("configuration of %s too large: %d bytes", v.Name, buf.Len())
	}
	// Zeros after the end of a tar archive are allowed.
	return append(buf.Bytes(), make([]byte, configSize-buf.Len())...), nil
}

const shelleyConfig = "/home/exedev/.config/shelley/boxe.json"

// configSize is the size of every config disk: a fork swaps it while the VM
// runs, and the VM keeps the size it saw at boot.
const configSize = 1 << 20

// agentsMD tells the coding agents in the VM (Claude, Codex, opencode,
// Shelley: the image links their instruction files here) where they are.
func (h *Hypervisor) agentsMD(v VM) string {
	web := "- There is no web front end: reach servers in the VM with `ssh -L`."
	if h.WebDomain != "" {
		web = fmt.Sprintf("- Web: an HTTP server on port 8000 is published at https://%[1]s.%[2]s, ports 3000-9999 at https://%[1]s--<port>.%[2]s (e.g. https://%[1]s--3000.%[2]s). HTTPS ends at the proxy: serve plain HTTP and listen on 0.0.0.0, not 127.0.0.1.\n"+
			"- Only logged-in members of the team reach these URLs. The proxy sends the user's email in the `X-Box-Email` header (it drops any `X-Box-*` header sent by clients) and `X-Forwarded-Proto: https`. Visitors with a share link (`ssh boxe share`) come without `X-Box-Email`.\n"+
			"- A desktop with Google Chrome: `sudo systemctl start boxe-desktop`, then run graphical programs with `DISPLAY=:1` (e.g. a headed browser to try a UI). Your user watches it at https://%[1]s--desktop.%[2]s.", v.Name, h.WebDomain)
	}
	llm := "- Shelley uses the models and API keys its user added in it."
	if h.LLMGateway != "" {
		llm = fmt.Sprintf("- Models: the API keys of the user's boxe account are added by the host to requests through its gateway. Login shells get `ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY` and `OPENAI_BASE_URL`/`OPENAI_API_KEY` for the providers with a key (the key is a placeholder; the host puts the real one in); Shelley uses the gateway too. If they are missing, the user adds a key from their computer with `ssh boxe llm set anthropic` (or `openai`); `ssh boxe llm usage` shows what was used. The gateway is %s.", h.LLMGateway)
	}
	sleep := ""
	if h.SleepDir != "" {
		sleep = fmt.Sprintf("- When nobody uses it for a while, the VM goes to sleep: its memory is saved and everything in it pauses. The next SSH connection or web request wakes it up where it was, in about a second. Cron jobs and systemd timers keep working: the VM wakes up shortly before each one and goes back to sleep after. A server that must answer without that delay, or a program that must keep running (a bot, a queue worker), needs `ssh boxe keep-awake %s on`, run from the user's computer.\n", v.Name)
	}
	return fmt.Sprintf("You are running in a boxe VM named %[1]s: a microVM on the team's own infrastructure. It is similar to exe.dev, but `ssh exe.dev` is not available here.\n\n"+
		"- Ubuntu, user `exedev` with passwordless sudo, persistent disk. Docker works.\n"+
		"- Internet works, unless your user restricted this VM to its integrations (`ssh boxe egress`, from their computer): then nothing else answers and only integration names resolve. The team's LAN, the host and the other VMs are not reachable from here.\n"+
		"- Integrations: your user keeps API secrets on the host and attaches them to this VM (`ssh boxe integrations`, from their computer). Call one at http://<integration>.int.%[3]s/<path>, plain HTTP (e.g. `curl http://stripe.int.%[3]s/v1/customers`): the host adds the credentials on the way out, so you never see the keys; do not ask for them nor send your own. An integration not attached to this VM answers 403.\n"+
		"- GitHub, if your user attached it: `git clone http://github.int.%[3]s/OWNER/REPO` (fetch and push too, unless it is read-only) and the API at http://github.int.%[3]s/api/ (e.g. `curl http://github.int.%[3]s/api/user`).\n"+
		"%[4]s\n%[2]s\n%[5]s"+
		"- VMs are created, copied and deleted from the user's computer with `ssh boxe` (`ssh boxe help`), not from inside the VM.\n", v.Name, web, h.SSHSuffix, llm, sleep)
}

// newHostKey makes the VM's SSH host key and its certificate, signed by
// HostCA for the name users type, <vm>.boxe.
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
// to bring VMs back after a host reboot. A sleeping VM stays asleep until
// someone needs it (Wake).
func (h *Hypervisor) Start(v VM) error {
	if h.Running(v.Name) {
		return h.guard(v) // net-setup.sh may have run again since
	}
	if h.Sleeping(v.Name) {
		return nil
	}
	return h.start(v, false)
}

// start runs v's hypervisor: a fresh boot, or with restore its memory as it
// was when it went to sleep.
func (h *Hypervisor) start(v VM, restore bool) error {
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
		// Known MAC: the host does not wait for ARP, which retries only once a second.
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
	os.RemoveAll(vmRunDir(v.Name))
	if err := os.Mkdir(vmRunDir(v.Name), 0o700); err != nil {
		return err
	}
	// The hypervisor is the part a VM could break out to: it runs as an
	// unprivileged user, sees only its own disks, sockets and saved memory,
	// and cannot use more memory and CPU than the VM was given (plus room
	// for itself).
	props := []string{
		fmt.Sprintf("MemoryMax=%dM", mib+mib/8+256),
		fmt.Sprintf("CPUQuota=%d%%", v.CPUs*100),
		"TasksMax=1024",
		"NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=yes", "PrivateTmp=yes",
		"ProtectKernelModules=yes", "ProtectKernelLogs=yes", "ProtectControlGroups=yes",
		"CapabilityBoundingSet=", "InaccessiblePaths=-/etc/boxe",
		"TemporaryFileSystem=" + runDir + ":ro", "BindPaths=" + vmRunDir(v.Name),
		"DevicePolicy=closed", "DeviceAllow=/dev/kvm rw", "DeviceAllow=/dev/net/tun rw", "DeviceAllow=" + disk + " rw",
	}
	if restore {
		// After a crash, not again from the same memory: the disk has moved on.
		props = append(props, "Restart=no")
	} else {
		props = append(props, "Restart=on-failure", "RestartSec=2")
	}
	if h.StateDir != "" {
		props = append(props, "TemporaryFileSystem="+h.StateDir+":ro",
			"BindReadOnlyPaths="+h.Kernel, "BindReadOnlyPaths="+h.config(v.Name))
	}
	if h.SleepDir != "" {
		// Its own sleep directory, writable, and none of the others'.
		dir := h.sleepDir(v.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if h.VMUserID > 0 {
			if err := os.Chown(dir, h.VMUserID, h.VMGroupID); err != nil {
				return err
			}
		}
		props = append(props, "TemporaryFileSystem="+h.SleepDir+":ro", "BindPaths="+dir)
	}
	if h.Boxed != "" {
		// Stopping the unit (restart, rm, host shutdown) first presses the
		// power button and waits for the guest; systemd kills what is left.
		props = append(props, "ExecStop="+h.Boxed+" -power-off "+sock(v.Name),
			fmt.Sprintf("TimeoutStopSec=%d", int((powerOffWait+10*time.Second).Seconds())))
	}
	if h.VMUserID > 0 {
		for _, p := range []string{disk, h.config(v.Name)} {
			if err := os.Chown(p, h.VMUserID, h.VMGroupID); err != nil {
				return err
			}
		}
		if err := os.Chown(vmRunDir(v.Name), h.VMUserID, h.VMGroupID); err != nil {
			return err
		}
	}
	if h.VMUser != "" {
		props = append(props, "User="+h.VMUser, "Group="+h.VMUser, "SupplementaryGroups=kvm")
	}
	args := []string{"-q", "--unit", unit(v.Name)}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	if restore {
		if err := h.retarget(v, disk); err != nil {
			return err
		}
		args = append(args, "/usr/local/bin/cloud-hypervisor", "--api-socket", sock(v.Name),
			"--restore", "source_url=file://"+h.sleepDir(v.Name)+",memory_restore_mode=copyonwrite,resume=false")
		return h.run("systemd-run", args...)
	}
	cmdline := fmt.Sprintf("console=ttyS0 root=/dev/vda rw init=/usr/local/bin/boxe-init ip=%s::10.42.0.1:255.255.0.0:%s:eth0:off quiet", v.IP(), v.Name)
	args = append(args, "/usr/local/bin/cloud-hypervisor",
		"--api-socket", sock(v.Name),
		"--kernel", h.Kernel,
		"--cmdline", cmdline,
		"--disk", "path="+disk+",image_type=raw",
		"path="+h.config(v.Name)+",image_type=raw,readonly=on",
		"--cpus", fmt.Sprintf("boot=%d", v.CPUs),
		"--memory", "size="+v.Mem,
		"--net", "tap="+t+",mac="+v.MAC(),
		// The host talks to boxe-agent in the VM through it (see sleep.go).
		"--vsock", "cid=3,socket="+vsockPath(v.Name),
		// The console goes to the journal (journalctl -u boxe-vm-<name>),
		// which caps its size, instead of a file that grows forever.
		"--serial", "tty",
		"--console", "off")
	return h.run("systemd-run", args...)
}

// Stop powers v off: the unit asks the guest to shut down (see Start) and
// cuts the power if it does not within powerOffWait. A sleeping VM forgets
// its memory: it boots afresh next time.
func (h *Hypervisor) Stop(v VM) error {
	if err := h.forgetSleep(v.Name); err != nil {
		return err
	}
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
	if err := h.run("nft", fmt.Sprintf(`add element bridge boxe mac { "%s" . %s }`, tap(v), v.MAC())); err != nil {
		return err
	}
	if err := h.run("nft", fmt.Sprintf(`add element bridge boxe ip4 { "%s" . %s }`, tap(v), v.IP())); err != nil {
		return err
	}
	return h.egress(v)
}

// egress closes the VM's tap to all but the gateway (net-setup.sh), or
// opens it, as v.Egress says. Opening is best effort: a tap left closed
// is safe.
func (h *Hypervisor) egress(v VM) error {
	if v.Egress == egressIntegrations {
		return h.run("nft", fmt.Sprintf(`add element bridge boxe closed { "%s" }`, tap(v)))
	}
	h.run("nft", fmt.Sprintf(`delete element bridge boxe closed { "%s" }`, tap(v)))
	return nil
}

// State is running, starting, failed, sleeping or stopped.
func (h *Hypervisor) State(name string) string {
	out, _ := h.exec(5*time.Second, "systemctl", "is-active", unit(name))
	switch strings.TrimSpace(string(out)) {
	case "active", "reloading":
		return "running"
	case "activating":
		return "starting"
	}
	if h.Sleeping(name) {
		return "sleeping"
	}
	if strings.TrimSpace(string(out)) == "failed" {
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
			return hostErrorf("%s stopped while booting (journalctl -u %s on the host)", v.Name, unit(v.Name))
		}
		c, err := h.dial("tcp", v.IP()+":22", 300*time.Millisecond)
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
	return hostErrorf("%s: no SSH after %s", v.Name, timeout)
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
		// A copy's disk is a clone of a snapshot of its source, taken for it.
		origin := ""
		if out, err := h.exec(30*time.Second, "zfs", "get", "-H", "-o", "value", "origin", h.ds(v.Name)); err == nil {
			origin = strings.TrimSpace(string(out))
		}
		defer func() {
			if strings.HasPrefix(origin, h.Dataset+"/") && strings.Contains(origin, "@cp-"+v.Name+"-") && !h.diskExists(v.Name) {
				h.run("zfs", "destroy", origin) // fails, harmlessly, if another copy was made from it
			}
		}()
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
	h.run("nft", fmt.Sprintf(`delete element bridge boxe mac { "%s" . %s }`, tap(v), v.MAC()))
	h.run("nft", fmt.Sprintf(`delete element bridge boxe ip4 { "%s" . %s }`, tap(v), v.IP()))
	h.run("nft", fmt.Sprintf(`delete element bridge boxe closed { "%s" }`, tap(v)))
	h.run("ip", "neigh", "del", v.IP(), "dev", "br0")
	os.RemoveAll(vmRunDir(v.Name))
	os.Remove(h.config(v.Name))
	if h.SleepDir != "" {
		os.RemoveAll(h.sleepDir(v.Name))
	}
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

// powerOffWait is how long a guest gets to shut down cleanly.
const powerOffWait = 30 * time.Second

// powerOff presses the power button of the VM behind sock and waits until
// its hypervisor, pid, exits. Run by systemd as the unit's ExecStop.
func powerOff(sock string, pid int, wait time.Duration) error {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	req, _ := http.NewRequest("PUT", "http://vm/api/v1/vm.power-button", nil)
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("power button: %s", res.Status)
	}
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
	}
	return fmt.Errorf("the guest did not shut down within %v", wait)
}
