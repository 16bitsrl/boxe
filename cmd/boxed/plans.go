package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Subscriptions: a member's own Claude or ChatGPT plan, used by Claude
// Code and Codex in their VMs as an API key is used by everything else.
// As with the keys, the credentials stay on the host: the agents in the VM
// send a placeholder and the gateway signs their requests in.
//
// Claude: the long-lived token of `claude setup-token`. Claude Code, given
// CLAUDE_CODE_OAUTH_TOKEN=boxe, sends it as a bearer with the OAuth beta
// header, which is how the gateway tells its requests from those with an
// API key's placeholder.
//
// ChatGPT: a Codex login (auth.json) made for boxe alone. Its refresh token
// changes at every refresh, and a used one no longer works, so boxed must be
// the only one to refresh it: it does so as soon as the login is saved, and
// whenever the access token is about to expire. Codex in the VM is set up
// with a provider on /chatgpt, which goes to the backend Codex uses with a
// ChatGPT login.

// claudeOAuthBeta is the beta header of requests signed in with a Claude plan.
const claudeOAuthBeta = "oauth-2025-04-20"

// claudeCodeSignedIn says whether r is Claude Code's, signed in with a
// Claude subscription (or the placeholder of one).
func claudeCodeSignedIn(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") &&
		strings.Contains(r.Header.Get("Anthropic-Beta"), claudeOAuthBeta)
}

// setClaude asks for the token of a Claude subscription and checks it.
func (b *Box) setClaude(ask func(string) (string, error)) (string, error) {
	token, err := ask("On your computer, run claude setup-token and sign in with your Claude account.\n" +
		"Paste the token it prints, then press Enter (with ssh -t it is not shown): ")
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "sk-ant-oat") || len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("that does not look like the token of claude setup-token (sk-ant-oat...)")
	}
	p, _ := llmProviderNamed("claude")
	return token, b.llm.check(p, token)
}

// chatgptTokenURL refreshes the logins of Codex, whose client boxed is.
const chatgptTokenURL = "https://auth.openai.com/oauth/token"

const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// chatgptRefreshMargin is how long before it expires an access token is
// refreshed.
const chatgptRefreshMargin = 5 * time.Minute

// chatgptLogin is a ChatGPT login of Codex, as boxed keeps it.
type chatgptLogin struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Account string `json:"account_id"`
	Expires int64  `json:"expires"` // of Access, Unix seconds
}

// setChatGPT reads the auth.json of a Codex login, made for boxe, and
// takes the login over by refreshing it.
func (b *Box) setChatGPT(ask func(string) (string, error)) (string, error) {
	text, err := ask(fmt.Sprintf("On your computer, sign in to Codex for boxe alone, in a folder of its own:\n"+
		"  export CODEX_HOME=$(mktemp -d); codex login --device-auth\n"+
		"then: ssh %s llm set chatgpt < $CODEX_HOME/auth.json\n"+
		"(not your usual ~/.codex/auth.json: boxe refreshes the login, and the copy elsewhere stops working)\n", b.name))
	// auth.json spans several lines: read until it is whole.
	for i := 0; err == nil && strings.HasPrefix(strings.TrimSpace(text), "{") && !json.Valid([]byte(text)) && i < 1000; i++ {
		var more string
		more, err = ask("")
		text += "\n" + more
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	var auth struct {
		Tokens struct {
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal([]byte(text), &auth) != nil || auth.Tokens.Refresh == "" || auth.Tokens.Account == "" {
		return "", fmt.Errorf("that is not the auth.json of a Codex login with ChatGPT")
	}
	login, err := b.llm.refreshChatGPT(auth.Tokens.Refresh)
	if err != nil {
		return "", err
	}
	login.Account = auth.Tokens.Account
	saved, _ := json.Marshal(login)
	return string(saved), nil
}

// errLoginRefused is a refresh token OpenAI no longer takes.
var errLoginRefused = errors.New("OpenAI refused the login: sign in again")

// refreshChatGPT trades a refresh token for new tokens: the old one no
// longer works after this.
func (g *Gateway) refreshChatGPT(refresh string) (chatgptLogin, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id": codexClientID, "grant_type": "refresh_token", "refresh_token": refresh, "scope": "openid profile email",
	})
	req, _ := http.NewRequest("POST", g.chatgptTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return chatgptLogin{}, fmt.Errorf("could not reach OpenAI to refresh the login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return chatgptLogin{}, errLoginRefused
	}
	if resp.StatusCode != http.StatusOK {
		return chatgptLogin{}, fmt.Errorf("could not refresh the login with OpenAI: %s", resp.Status)
	}
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens); err != nil || tokens.Access == "" {
		return chatgptLogin{}, fmt.Errorf("OpenAI answered the refresh without tokens")
	}
	if tokens.Refresh == "" {
		tokens.Refresh = refresh
	}
	return chatgptLogin{Access: tokens.Access, Refresh: tokens.Refresh, Expires: jwtExpiry(tokens.Access)}, nil
}

// jwtExpiry is when the JWT t expires, read without checking it (it is
// only sent on to whom made it); in an hour if it does not say.
func jwtExpiry(t string) int64 {
	parts := strings.Split(t, ".")
	if len(parts) == 3 {
		if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct{ Exp int64 }
			if json.Unmarshal(b, &claims) == nil && claims.Exp > 0 {
				return claims.Exp
			}
		}
	}
	return time.Now().Add(time.Hour).Unix()
}

// chatgptAccess is the access token of user's ChatGPT login and its
// account, refreshed first if it is about to expire. One request at a time
// refreshes it: the others wait and take the new one.
func (g *Gateway) chatgptAccess(user string) (string, string, error) {
	m := g.chatgptLock(user)
	m.Lock()
	defer m.Unlock()
	keys, err := g.store.LLMKeys(user)
	if err != nil {
		return "", "", err
	}
	var login chatgptLogin
	if err := json.Unmarshal([]byte(keys["chatgpt"]), &login); err != nil {
		return "", "", fmt.Errorf("the saved login is broken: %v", err)
	}
	if time.Until(time.Unix(login.Expires, 0)) > chatgptRefreshMargin {
		return login.Access, login.Account, nil
	}
	fresh, err := g.refreshChatGPT(login.Refresh)
	if err != nil {
		return "", "", err
	}
	fresh.Account = login.Account
	saved, _ := json.Marshal(fresh)
	// The old refresh token is spent: without the new one the login is lost.
	if err := g.store.SetLLMKey(user, "chatgpt", string(saved)); err != nil {
		return "", "", fmt.Errorf("could not save the refreshed login: %v", err)
	}
	return fresh.Access, fresh.Account, nil
}

// chatgptLock is held while user's ChatGPT login changes: refreshed, or
// replaced and removed by its owner, so a refresh never brings back a
// login just replaced or removed.
func (g *Gateway) chatgptLock(user string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.chatgpts[user]
	if m == nil {
		m = &sync.Mutex{}
		g.chatgpts[user] = m
	}
	return m
}

// loginExpired answers a request whose owner's ChatGPT login could not be
// refreshed.
func loginExpired(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	msg := "boxe could not refresh your ChatGPT login, try again: " + err.Error()
	status := http.StatusBadGateway
	if errors.Is(err, errLoginRefused) {
		msg = "your ChatGPT login on boxe no longer works: sign in again for boxe and run ssh boxe llm set chatgpt (ssh boxe llm set chatgpt tells how)"
		status = http.StatusUnauthorized
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "authentication_error", "message": msg}})
}
