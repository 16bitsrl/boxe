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
	emails map[string]string // user -> email at the IdP, if given
	admins map[string]bool   // users marked admin in usersFile
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

// Email is user's email at the IdP, from the users file; "" if not given.
func (k *Keys) Email(user string) string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.emails[user]
}

// Member is the user whose email at the IdP is email, and whether they are
// an admin; "" if the users file links no user to it.
func (k *Keys) Member(email string) (user string, admin bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for u, e := range k.emails {
		if e != "" && e == strings.ToLower(email) {
			return u, k.admins[u]
		}
	}
	return "", false
}

func (k *Keys) Refresh() {
	users, emails, admins, err := readUsers(k.usersFile)
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
	k.mu.Lock()
	k.emails, k.admins = emails, admins
	k.mu.Unlock()
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

// readUsers reads the users file: one GitHub username per line, optionally
// followed by the member's email at the IdP and "admin" for an admin.
func readUsers(path string) (users []string, emails map[string]string, admins map[string]bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()
	emails, admins = map[string]string{}, map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		users = append(users, fields[0])
		if len(fields) > 1 {
			emails[fields[0]] = strings.ToLower(fields[1])
		}
		if len(fields) > 2 && fields[2] == "admin" {
			admins[fields[0]] = true
		}
	}
	return users, emails, admins, sc.Err()
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
