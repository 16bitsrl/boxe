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

// The config disk is a tar archive that box-init (image/box-init) extracts
// over the VM's root at boot.
func TestConfigArchive(t *testing.T) {
	ca, err := loadHostKey(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Hypervisor{HostPubKey: "ssh-ed25519 HOST exe-host\n", HostCA: ca,
		SSHSuffix: "box", WebDomain: "box.example", DNS: []string{"9.9.9.9"}}
	v := VM{Name: "quick-fox", Index: 2}
	data, err := h.configArchive(v, "ssh-ed25519 USER u\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%512 != 0 {
		t.Errorf("size %d is not a multiple of 512", len(data))
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
	if e := check("etc/hostname", 0o644, 0, tar.TypeReg); e.contents != "quick-fox\n" {
		t.Errorf("hostname: %q", e.contents)
	}
	if e := check("etc/hosts", 0o644, 0, tar.TypeReg); !strings.Contains(e.contents, "127.0.1.1 quick-fox") {
		t.Errorf("hosts: %q", e.contents)
	}
	if e := check("etc/resolv.conf", 0o644, 0, tar.TypeReg); e.contents != "nameserver 9.9.9.9\n" {
		t.Errorf("resolv.conf: %q", e.contents)
	}
	// What the VM shows and tells its coding agents comes from boxd's
	// settings, not from the image.
	if e := check("etc/box/web-domain", 0o644, 0, tar.TypeReg); e.contents != "box.example\n" {
		t.Errorf("web-domain: %q", e.contents)
	}
	agents := check("home/exedev/.config/box/AGENTS.md", 0o644, vmUID, tar.TypeReg).contents
	for _, want := range []string{"https://quick-fox.box.example", "https://quick-fox--<port>.box.example", "ssh box help"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md lacks %q", want)
		}
	}
	if e := check("etc/machine-id", 0o444, 0, tar.TypeReg); len(e.contents) != 33 {
		t.Errorf("machine-id: %q", e.contents)
	}
	if e := check("etc/ssh/ssh_host_ed25519_key", 0o600, 0, tar.TypeReg); !strings.Contains(e.contents, "OPENSSH PRIVATE KEY") {
		t.Error("host key is not a private key")
	}
	pub := check("etc/ssh/ssh_host_ed25519_key.pub", 0o644, 0, tar.TypeReg).contents

	// The host certificate lets clients trust every VM through one
	// @cert-authority line, for the name they type: <vm>.box.
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
	if err := checker.CheckHostKey("quick-fox.box:22", nil, cert); err != nil {
		t.Errorf("certificate rejected for quick-fox.box: %v", err)
	}
	if checker.CheckHostKey("other.box:22", nil, cert) == nil {
		t.Error("certificate accepted for another VM")
	}
	if !bytes.Equal(cert.Key.Marshal(), hostKey.Marshal()) {
		t.Error("certificate is not for the VM's host key")
	}
	check("home/exedev/.ssh", 0o700, vmUID, tar.TypeDir)
	if e := check("home/exedev/.ssh/authorized_keys", 0o600, vmUID, tar.TypeReg); e.contents != "ssh-ed25519 USER u\nssh-ed25519 HOST exe-host\n" {
		t.Errorf("authorized_keys: %q", e.contents)
	}
	id := check("etc/box/id", 0o644, 0, tar.TypeReg).contents

	// Every archive is new: a copy must not look already applied.
	again, _ := h.configArchive(v, "ssh-ed25519 USER u\n")
	tr = tar.NewReader(bytes.NewReader(again))
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatal("no etc/box/id in the second archive")
		}
		if hdr.Name == "etc/box/id" {
			if b, _ := io.ReadAll(tr); string(b) == id {
				t.Error("two archives with the same id")
			}
			break
		}
	}
}
