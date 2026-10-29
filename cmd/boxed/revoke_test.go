package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Someone removed from the team (or whose key is gone from GitHub) is
// disconnected at the next key refresh, jumps into VMs included.
func TestSSHRevokedKeyIsDisconnected(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(sk)
	keys := &Keys{store: s}
	keys.set(map[string]string{string(signer.PublicKey().Marshal()): "alice"}, nil)
	b := &Box{store: s, keys: keys}
	srv := b.server()
	hostKey, _ := loadHostKey(filepath.Join(t.TempDir(), "host"))
	srv.AddHostKey(hostKey)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{User: "alice",
		Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b.revalidate() // nothing changed: stays connected
	if _, _, err := c.SendRequest("keepalive@boxe", true, nil); err != nil {
		t.Fatalf("disconnected although the key is still valid: %v", err)
	}

	keys.set(map[string]string{}, nil)
	b.revalidate()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("still connected after the key was revoked")
	}
}

// When GitHub does not answer, cached keys are used, but not for ever.
func TestStaleCachedKeysExpire(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SaveKeys("alice", "ssh-ed25519 AAAA alice")
	if keys, ok := s.CachedKeys("alice", time.Hour); !ok || keys == "" {
		t.Error("fresh cached keys not used")
	}
	s.db.Exec("UPDATE user_keys SET updated_at = ?", time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339))
	if _, ok := s.CachedKeys("alice", maxKeyCacheAge); ok {
		t.Error("used keys cached more than a week ago")
	}
}
