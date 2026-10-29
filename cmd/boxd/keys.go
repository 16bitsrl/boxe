package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Keys maps SSH public keys to team members. The members are the GitHub
// usernames listed in usersFile; their keys come from github.com/<user>.keys.
type Keys struct {
	usersFile string
	store     *Store

	mu     sync.RWMutex
	byKey  map[string]string // marshalled public key -> user
	byUser map[string]string // user -> authorized_keys lines
	lastOK time.Time         // last refresh where GitHub answered for everyone
}

func (k *Keys) LastOK() time.Time {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.lastOK
}

func (k *Keys) User(key gossh.PublicKey) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	u, ok := k.byKey[string(key.Marshal())]
	return u, ok
}

func (k *Keys) userOf(marshalledKey string) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	u, ok := k.byKey[marshalledKey]
	return u, ok
}

func (k *Keys) AuthorizedKeys(user string) string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.byUser[user]
}

func (k *Keys) Refresh() {
	users, err := readUsers(k.usersFile)
	if err != nil {
		log.Printf("keys: %v", err)
		return
	}
	byKey := map[string]string{}
	byUser := map[string]string{}
	allOK := true
	for _, u := range users {
		keys, err := fetchGitHubKeys(u)
		if err != nil {
			allOK = false
			// Keep working with the last keys we saw for this user, for a while:
			// a key removed from GitHub must stop working even if GitHub is down.
			var ok bool
			keys, ok = k.store.CachedKeys(u, maxKeyCacheAge)
			log.Printf("keys: %s: %v, using cached keys: %v", u, err, ok)
		} else if err := k.store.SaveKeys(u, keys); err != nil {
			log.Printf("keys: saving %s: %v", u, err)
		}
		for _, line := range strings.Split(keys, "\n") {
			pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				continue
			}
			byKey[string(pk.Marshal())] = u
			byUser[u] += strings.TrimSpace(line) + "\n"
		}
	}
	k.set(byKey, byUser)
	if allOK {
		k.mu.Lock()
		k.lastOK = time.Now()
		k.mu.Unlock()
	}
	log.Printf("keys: %d keys for %d users", len(byKey), len(users))
}

const maxKeyCacheAge = 7 * 24 * time.Hour

func (k *Keys) set(byKey, byUser map[string]string) {
	k.mu.Lock()
	k.byKey, k.byUser = byKey, byUser
	k.mu.Unlock()
}

func readUsers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var users []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			users = append(users, line)
		}
	}
	return users, sc.Err()
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func fetchGitHubKeys(user string) (string, error) {
	resp, err := httpClient.Get("https://github.com/" + user + ".keys")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
