package main

import (
	"net/http"
	"testing"
)

// https://<vm>--desktop.<domain> is the VM's desktop (noVNC): like the
// agent, its owner's only.
func TestWebDesktopIsTheOwners(t *testing.T) {
	wt := newWebTest(t)
	owners := map[string]string{"alice": "alice@example.com"}
	wt.web.ownerEmail = func(u string) string { return owners[u] }
	b := wt.browser("member")
	if res := b.get("https://test--desktop.boxe.example/", nil); res.StatusCode != 200 || wt.port != desktopWebPort {
		t.Fatalf("owner: %d, port %d", res.StatusCode, wt.port)
	}
	owners["alice"] = "boss@example.com"
	wt.seen = nil
	if res := b.get("https://test--desktop.boxe.example/", nil); res.StatusCode != http.StatusForbidden || wt.seen != nil {
		t.Errorf("another member: %d", res.StatusCode)
	}
	if res := b.do("https://test--2998.boxe.example/", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("desktop as a port: %d, want 404", res.StatusCode)
	}
}

// noVNC and Shelley talk over WebSockets, which a page of another VM (same
// site, so the owner's cookie goes along) could open: only the VM host's
// own pages may.
func TestWebOwnerOnlyWebSockets(t *testing.T) {
	wt := newWebTest(t)
	wt.web.ownerEmail = func(string) string { return "alice@example.com" }
	b := wt.browser("member")
	b.get("https://test--desktop.boxe.example/", nil)
	b.get("https://test--agent.boxe.example/", nil)
	ws := func(origin string) http.Header {
		return http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Origin": {origin}}
	}
	for _, host := range []string{"test--desktop", "test--agent"} {
		u := "https://" + host + ".boxe.example/websockify"
		if res := b.do(u, ws("https://"+host+".boxe.example")); res.StatusCode == http.StatusForbidden {
			t.Errorf("%s from its own page: 403", host)
		}
		for _, origin := range []string{"https://other.boxe.example", "https://evil.example", ""} {
			wt.seen = nil
			if res := b.do(u, ws(origin)); res.StatusCode != http.StatusForbidden || wt.seen != nil {
				t.Errorf("%s from %q: %d, want 403", host, origin, res.StatusCode)
			}
		}
	}
	// Apps on other ports handle their own WebSockets.
	b.get("https://test--3000.boxe.example/", nil)
	if res := b.do("https://test--3000.boxe.example/ws", ws("https://other.boxe.example")); res.StatusCode == http.StatusForbidden {
		t.Error("port 3000: 403")
	}
}
