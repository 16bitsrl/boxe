package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// githubAt sends the requests of fetchGitHubKeys to h for the test.
func githubAt(t *testing.T, h http.HandlerFunc) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := httpClient
	httpClient = &http.Client{Transport: rewriteTo{srv.URL}}
	t.Cleanup(func() { httpClient = old })
}

type rewriteTo struct{ base string }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL, _ = out.URL.Parse(r.base + req.URL.Path)
	out.Host = ""
	return http.DefaultTransport.RoundTrip(out)
}

// GitHub failing for one member says who, since when and why, on the
// admin's page; the others go on. Once it answers again, it is forgotten.
func TestKeysErrorPerMember(t *testing.T) {
	broken := true
	githubAt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bob.keys" && broken {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTestsOnly000000000000000000000 x\n"))
	})
	s, err := openStore(filepath.Join(t.TempDir(), "boxe.db"))
	if err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(t.TempDir(), "users")
	os.WriteFile(users, []byte("alice\nbob\n"), 0o644)
	k := &Keys{usersFile: users, store: s}
	k.Refresh()
	k.Refresh()
	m := map[string]teamMember{}
	for _, x := range k.Members() {
		m[x.User] = x
	}
	if m["alice"].KeyError != "" || !m["alice"].KeyErrorSince.IsZero() {
		t.Errorf("alice: %+v", m["alice"])
	}
	bob := m["bob"]
	if !strings.Contains(bob.KeyError, "404") || bob.KeyErrorSince.IsZero() || time.Since(bob.KeyErrorSince) > time.Minute {
		t.Errorf("bob: %+v", bob)
	}
	// The first failure counts, not the latest.
	first := bob.KeyErrorSince
	k.Refresh()
	for _, x := range k.Members() {
		if x.User == "bob" && !x.KeyErrorSince.Equal(first) {
			t.Errorf("since moved: %v, was %v", x.KeyErrorSince, first)
		}
	}
	broken = false
	k.Refresh()
	for _, x := range k.Members() {
		if x.KeyError != "" {
			t.Errorf("%s still: %q", x.User, x.KeyError)
		}
	}
}

func TestKeyProblem(t *testing.T) {
	for ago, want := range map[time.Duration]string{0: "1m", 90 * time.Minute: "1h30m", 3 * time.Hour: "3h", 20 * time.Minute: "20m"} {
		m := teamMember{KeyError: "github: 404 Not Found", KeyErrorSince: time.Now().Add(-ago)}
		if got := m.KeyProblem(); got != "GitHub: 404 Not Found, for "+want {
			t.Errorf("%v ago: %q", ago, got)
		}
	}
}
