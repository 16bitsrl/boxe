package main

import (
	"archive/tar"
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// The config disk is a tar archive that boxe-init (image/boxe-init) extracts
// over the VM's root at boot.
func TestConfigArchive(t *testing.T) {
	ca, err := loadHostKey(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Hypervisor{HostPubKey: "ssh-ed25519 HOST host\n", HostCA: ca,
		SSHSuffix: "boxe", WebDomain: "boxe.example"}
	v := VM{Name: "quick-fox", Index: 2}
	data, err := h.configArchive(v, "ssh-ed25519 USER u\n")
	if err != nil {
		t.Fatal(err)
	}
	// Always the same size: a fork swaps the disk while the VM runs, and
	// the VM keeps seeing the size it booted with.
	if len(data) != configSize {
		t.Errorf("size %d, want %d", len(data), configSize)
	}
	type entry struct {
		mode     int64
		uid      int
		typ      byte
		contents string
	}
	got := map[string]entry{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = entry{hdr.Mode, hdr.Uid, hdr.Typeflag, string(b)}
	}
	check := func(name string, mode int64, uid int, typ byte) entry {
		e, ok := got[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if e.mode != mode || e.uid != uid || e.typ != typ {
			t.Errorf("%s: mode %o uid %d type %c, want %o %d %c", name, e.mode, e.uid, e.typ, mode, uid, typ)
		}
		return e
	}
	// The full name: Shelley adds .exe.xyz to a hostname without dots.
	if e := check("etc/hostname", 0o644, 0, tar.TypeReg); e.contents != "quick-fox.boxe.example\n" {
		t.Errorf("hostname: %q", e.contents)
	}
	if e := check("etc/hosts", 0o644, 0, tar.TypeReg); !strings.Contains(e.contents, "127.0.1.1 quick-fox.boxe.example quick-fox\n") {
		t.Errorf("hosts: %q", e.contents)
	}
	// boxed's own DNS server, which knows the integrations' names.
	if e := check("etc/resolv.conf", 0o644, 0, tar.TypeReg); e.contents != "nameserver 10.42.0.1\n" {
		t.Errorf("resolv.conf: %q", e.contents)
	}
	// What the VM shows and tells its coding agents comes from boxed's
	// settings, not from the image.
	if e := check("etc/boxe/web-domain", 0o644, 0, tar.TypeReg); e.contents != "boxe.example\n" {
		t.Errorf("web-domain: %q", e.contents)
	}
	agents := check("home/exedev/.config/boxe/AGENTS.md", 0o644, vmUID, tar.TypeReg).contents
	for _, want := range []string{"https://quick-fox.boxe.example", "https://quick-fox--<port>.boxe.example", "ssh boxe help",
		"http://<integration>.int.boxe/", "git clone http://github.int.boxe/OWNER/REPO", "ssh boxe egress"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md lacks %q", want)
		}
	}
	// What boxe-agent sets up after a fork; at boot the kernel does it.
	if e := check("etc/boxe/net", 0o644, 0, tar.TypeReg); e.contents != "ADDRESS=10.42.0.2/16\nGATEWAY=10.42.0.1\nMAC=02:42:0a:2a:00:02\n" {
		t.Errorf("net: %q", e.contents)
	}
	if e := check("etc/machine-id", 0o444, 0, tar.TypeReg); len(e.contents) != 33 {
		t.Errorf("machine-id: %q", e.contents)
	}
	if e := check("etc/ssh/ssh_host_ed25519_key", 0o600, 0, tar.TypeReg); !strings.Contains(e.contents, "OPENSSH PRIVATE KEY") {
		t.Error("host key is not a private key")
	}
	pub := check("etc/ssh/ssh_host_ed25519_key.pub", 0o644, 0, tar.TypeReg).contents

	// The host certificate lets clients trust every VM through one
	// @cert-authority line, for the name they type: <vm>.boxe.
	certFile := check("etc/ssh/ssh_host_ed25519_key-cert.pub", 0o644, 0, tar.TypeReg).contents
	k, _, _, _, err := gossh.ParseAuthorizedKey([]byte(certFile))
	if err != nil {
		t.Fatal(err)
	}
	cert, ok := k.(*gossh.Certificate)
	if !ok {
		t.Fatalf("%T is not a certificate", k)
	}
	hostKey, _, _, _, _ := gossh.ParseAuthorizedKey([]byte(pub))
	checker := gossh.CertChecker{IsHostAuthority: func(auth gossh.PublicKey, _ string) bool {
		return bytes.Equal(auth.Marshal(), ca.PublicKey().Marshal())
	}}
	if err := checker.CheckHostKey("quick-fox.boxe:22", nil, cert); err != nil {
		t.Errorf("certificate rejected for quick-fox.boxe: %v", err)
	}
	if checker.CheckHostKey("other.boxe:22", nil, cert) == nil {
		t.Error("certificate accepted for another VM")
	}
	if !bytes.Equal(cert.Key.Marshal(), hostKey.Marshal()) {
		t.Error("certificate is not for the VM's host key")
	}
	check("home/exedev/.ssh", 0o700, vmUID, tar.TypeDir)
	if e := check("home/exedev/.ssh/authorized_keys", 0o600, vmUID, tar.TypeReg); e.contents != "ssh-ed25519 USER u\nssh-ed25519 HOST host\n" {
		t.Errorf("authorized_keys: %q", e.contents)
	}
	id := check("etc/boxe/id", 0o644, 0, tar.TypeReg).contents

	// Every archive is new: a copy must not look already applied.
	again, _ := h.configArchive(v, "ssh-ed25519 USER u\n")
	tr = tar.NewReader(bytes.NewReader(again))
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatal("no etc/boxe/id in the second archive")
		}
		if hdr.Name == "etc/boxe/id" {
			if b, _ := io.ReadAll(tr); string(b) == id {
				t.Error("two archives with the same id")
			}
			break
		}
	}
}

// Without a web domain, or when the full name is too long for the kernel (64
// characters), the VM keeps its short name.
func TestVMHostname(t *testing.T) {
	for _, c := range []struct{ name, domain, want string }{
		{"quick-fox", "boxe.example", "quick-fox.boxe.example"},
		{"quick-fox", "", "quick-fox"},
		{"quick-fox", strings.Repeat("d", 60), "quick-fox"},
	} {
		if got := vmHostname(c.name, c.domain); got != c.want {
			t.Errorf("vmHostname(%q, %q) = %q, want %q", c.name, c.domain, got, c.want)
		}
	}
}

// Where VMs sleep, the VM tells its user (the welcome message reads
// etc/boxe/sleeps) and its coding agents that scheduled jobs still run, and
// what keep-awake is for.
func TestConfigSaysVMsSleep(t *testing.T) {
	ca, _ := loadHostKey(filepath.Join(t.TempDir(), "ca"))
	h := &Hypervisor{HostCA: ca, SSHSuffix: "boxe"}
	files := func() map[string]string {
		data, err := h.configArchive(VM{Name: "quick-fox", Index: 2}, "")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		tr := tar.NewReader(bytes.NewReader(data))
		for hdr, err := tr.Next(); err == nil; hdr, err = tr.Next() {
			b, _ := io.ReadAll(tr)
			m[hdr.Name] = string(b)
		}
		return m
	}
	f := files()
	if _, ok := f["etc/boxe/sleeps"]; ok || strings.Contains(f["home/exedev/.config/boxe/AGENTS.md"], "sleep") {
		t.Error("VMs never sleep here, yet the VM says they do")
	}
	h.SleepDir = "/boxe/sleep"
	f = files()
	if _, ok := f["etc/boxe/sleeps"]; !ok {
		t.Error("no etc/boxe/sleeps")
	}
	for _, want := range []string{"sleep", "Cron jobs", "systemd timers", "keep-awake quick-fox on"} {
		if !strings.Contains(f["home/exedev/.config/boxe/AGENTS.md"], want) {
			t.Errorf("AGENTS.md lacks %q", want)
		}
	}
}
