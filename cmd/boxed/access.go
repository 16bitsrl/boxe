package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Access: the owner of a VM lets other members (or the whole team) use it
// as they do: SSH, exec, the agent, the desktop. Managing it (rm, restart,
// ttl, share, egress, integrations, cp) stays the owner's. Inside the VM,
// sshd asks boxe for the keys that may log in (sshdAccessConf), so access
// given or taken away counts at the next login.

// vmUser is the user members log into VMs as.
const vmUser = "boxedev"

// usable is the VM name if user may use it: theirs, or one they have
// access to.
func (b *Box) usable(user, name string) (VM, error) {
	v, err := b.store.Get(name)
	if err == errNotFound || (err == nil && !b.store.HasAccess(v, user)) {
		return v, fmt.Errorf("you have no VM named %s", name)
	}
	return v, err
}

// accessJSON is the answer of access --json.
type accessJSON struct {
	Name  string   `json:"name"`
	Owner string   `json:"owner"`
	Users []string `json:"users"` // besides the owner; "team": every member
}

// cmdAccess is `access <name>` (who may use it), and `access <name> add|rm
// <member|team>` for its owner.
func (b *Box) cmdAccess(o output, user string, args []string) error {
	usage := fmt.Errorf("usage: ssh %s access <name> [add|rm <member|team>]", b.name)
	if len(args) != 1 && (len(args) != 3 || (args[1] != "add" && args[1] != "rm")) {
		return usage
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var v VM
	var err error
	if len(args) == 1 {
		v, err = b.usable(user, args[0])
	} else {
		v, err = b.owned(user, args[0])
	}
	if err != nil {
		return err
	}
	if len(args) == 3 {
		who := args[2]
		switch {
		case who == v.Owner:
			return fmt.Errorf("%s is %s's already", v.Name, who)
		case who != teamAccess && !b.keys.InTeam(who):
			return fmt.Errorf("%s is not in the team", who)
		case args[1] == "add":
			if err := b.store.Grant(v, who); err != nil {
				return err
			}
			log.Printf("%s: gave %s access to %s", user, who, v.Name)
		default:
			had, err := b.store.Revoke(v.Name, who)
			if err != nil {
				return err
			}
			if !had {
				return fmt.Errorf("%s has no access to %s", who, v.Name)
			}
			log.Printf("%s: took %s's access to %s away", user, who, v.Name)
		}
	}
	users, err := b.store.Access(v)
	if err != nil {
		return err
	}
	if users == nil {
		users = []string{}
	}
	text := v.Name + ": " + v.Owner + " (owner)"
	for _, u := range users {
		if u == teamAccess {
			u = "the whole team"
		}
		text += ", " + u
	}
	if len(args) == 3 && len(users) > 0 {
		text += "\nThey log in with ssh " + v.Name + "." + b.hv.SSHSuffix + " and use what the VM can: its integrations and model keys are " + v.Owner + "'s."
	}
	o.print(accessJSON{v.Name, v.Owner, users}, text+"\n")
	return nil
}

// sshdAccessConf is the sshd configuration of new VMs: sshd asks boxe for
// the keys that may log in, besides those in authorized_keys.
func sshdAccessConf() string {
	return "# The keys of the members who may use this VM (ssh boxe access), from the host.\n" +
		"AuthorizedKeysCommand /usr/bin/curl -sf --noproxy * --connect-timeout 1 --max-time 3 http://" + gatewayIP() + "/ssh-keys?user=%u\n" +
		"AuthorizedKeysCommandUser nobody\n"
}

// serveSSHKeys answers the VM v's sshd with the keys that may log in as
// boxedev: the owner's and those of the members with access.
func (b *Box) serveSSHKeys(w http.ResponseWriter, r *http.Request, v VM) {
	if r.URL.Query().Get("user") != vmUser {
		return
	}
	users, err := b.store.Access(v)
	if err != nil {
		log.Printf("ssh keys of %s: %v", v.Name, err)
		http.Error(w, "could not read who has access", http.StatusInternalServerError)
		return
	}
	who := []string{v.Owner}
	for _, u := range users {
		if u == teamAccess {
			who = append(who, b.keys.Users()...)
			break
		}
		who = append(who, u)
	}
	seen := map[string]bool{}
	var keys strings.Builder
	for _, u := range who {
		// Only members: who left the team has no keys here.
		if !seen[u] && b.keys.InTeam(u) {
			seen[u] = true
			keys.WriteString(b.keys.AuthorizedKeys(u))
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, keys.String())
}
