package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// The HTTPS API: POST https://<domain>/exec with a token, the body a command
// line as typed after `ssh boxe`. It runs as over SSH (Box.run), as the
// token's member, and answers what the command printed. Tokens come from
// `ssh boxe token new`; boxe keeps only their hash.

const (
	tokenPrefix   = "boxe_"
	defaultExp    = 30 * 24 * time.Hour
	maxExp        = 365 * 24 * time.Hour
	maxAPILine    = 64 << 10        // bytes of a command line
	execTimeout   = 5 * time.Minute // a command in a VM
	apiTimeout    = execTimeout + time.Minute
	apiTimeoutMsg = "The command took too long; it may still be running.\n"
)

// apiCommands are the commands a token may be limited to: all but token,
// so that a token never makes or lists tokens.
func apiCommands() []string {
	var cmds []string
	for _, c := range commands {
		if c != "token" && c != "exit" {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// tokenHash is what the database keeps of a token.
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// tokenJSON is a token as token new and token ls --json show it; the
// token itself only once, from new.
type tokenJSON struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Token    string   `json:"token,omitempty"`
	Commands []string `json:"commands"` // empty: all
	Created  string   `json:"created"`
	Expires  string   `json:"expires"`
	LastUsed string   `json:"last_used,omitempty"`
}

func toJSON(t Token) tokenJSON {
	j := tokenJSON{ID: t.ID, Label: t.Label, Commands: t.Cmds, Created: timeCol(t.Created), Expires: timeCol(t.Expires), LastUsed: timeCol(t.Used)}
	if j.Commands == nil {
		j.Commands = []string{}
	}
	return j
}

var validLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

func (b *Box) cmdToken(o output, user string, args []string) error {
	usage := fmt.Errorf("usage: ssh %s token new [--label <label>] [--exp 30d] [--cmds ls,new,...], token ls, token rm <id|label>", b.name)
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "new":
		return b.tokenNew(o, user, args[1:])
	case "ls":
		if len(args) != 1 {
			return usage
		}
		return b.tokenLs(o, user)
	case "rm":
		if len(args) != 2 {
			return usage
		}
		t, err := b.store.DeleteToken(user, args[1])
		if err == errNotFound {
			return fmt.Errorf("you have no token %s", args[1])
		}
		if err != nil {
			return err
		}
		log.Printf("%s: deleted token %s", user, t.ID)
		o.print(done{t.ID, "deleted"}, "token "+t.ID+" deleted\n")
		return nil
	}
	return usage
}

func (b *Box) tokenNew(o output, user string, args []string) error {
	fs := flag.NewFlagSet("token new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("label", "", "")
	exp := fs.String("exp", "30d", "")
	cmds := fs.String("cmds", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("usage: ssh %s token new [--label <label>] [--exp 30d] [--cmds ls,new,...]", b.name)
	}
	if b.hv.WebDomain == "" {
		return fmt.Errorf("this installation has no web front end")
	}
	d, err := parseDays(*exp)
	if err != nil || d < time.Minute || d > maxExp {
		return fmt.Errorf("invalid --exp %q: a duration like 1h or 30d, at most 365d", *exp)
	}
	t := Token{User: user, Label: *label, Created: time.Now().UTC().Truncate(time.Second)}
	t.Expires = t.Created.Add(d)
	if t.Label != "" && !validLabel.MatchString(t.Label) {
		return fmt.Errorf("invalid label %q: letters, digits, dots, dashes and underscores, up to 40", t.Label)
	}
	if *cmds != "" {
		allowed := apiCommands()
		for _, c := range strings.Split(*cmds, ",") {
			if !slices.Contains(allowed, c) {
				return fmt.Errorf("%q is not a command a token may run: %s", c, strings.Join(allowed, ","))
			}
			if !slices.Contains(t.Cmds, c) {
				t.Cmds = append(t.Cmds, c)
			}
		}
	}
	list, err := b.store.Tokens(user)
	if err != nil {
		return err
	}
	for _, o := range list {
		if t.Label != "" && o.Label == t.Label {
			return fmt.Errorf("you already have a token labelled %s", t.Label)
		}
	}
	id, secret := make([]byte, 4), make([]byte, 32)
	rand.Read(id)
	rand.Read(secret)
	t.ID = hex.EncodeToString(id)
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	if err := b.store.AddToken(t, tokenHash(token)); err != nil {
		return err
	}
	log.Printf("%s: made token %s (%s), commands %q, until %s", user, t.ID, t.Label, strings.Join(t.Cmds, ","), timeCol(t.Expires))
	j := toJSON(t)
	j.Token = token
	what := "every command but token"
	if len(t.Cmds) > 0 {
		what = strings.Join(t.Cmds, ", ")
	}
	name := t.ID
	if t.Label != "" {
		name += " (" + t.Label + ")"
	}
	o.print(j, fmt.Sprintf("Token %s, for %s, until %s:\n\n  %s\n\nIt is shown only now. Use it as:\n\n"+
		"  curl -X POST https://%s/exec -H \"Authorization: Bearer %s\" -d 'ls --json'\n\nssh %s token rm %s deletes it.\n",
		name, what, t.Expires.Local().Format("2006-01-02 15:04"), token, b.hv.WebDomain, token, b.name, t.ID))
	return nil
}

func (b *Box) tokenLs(o output, user string) error {
	list, err := b.store.Tokens(user)
	if err != nil {
		return err
	}
	if o.json {
		js := []tokenJSON{}
		for _, t := range list {
			js = append(js, toJSON(t))
		}
		o.print(js, "")
		return nil
	}
	now := time.Now()
	day := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	w := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLABEL\tCOMMANDS\tCREATED\tEXPIRES\tLAST USED")
	for _, t := range list {
		label, cmds, exp := t.Label, strings.Join(t.Cmds, ","), day(t.Expires)
		if label == "" {
			label = "-"
		}
		if cmds == "" {
			cmds = "all"
		}
		if !t.Expires.After(now) {
			exp = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, label, cmds, day(t.Created), exp, day(t.Used))
	}
	return w.Flush()
}

// exitStatus is a command run in a VM that failed: what it exited with.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }

var execLine = regexp.MustCompile(`(?s)^\s*exec\s+(\S+)\s+(\S.*)$`)

// cmdExec is `exec <name> <command>`: the command runs in the member's VM
// as exedev, as with `ssh <name>.boxe <command>`, and what it printed comes
// back. The command is the rest of the line as typed, for the VM's shell.
func (b *Box) cmdExec(w io.Writer, user, line string) error {
	m := execLine.FindStringSubmatch(line)
	if m == nil {
		return fmt.Errorf("usage: ssh %s exec <name> <command>", b.name)
	}
	name, cmd := m[1], strings.TrimSpace(m[2])
	if _, err := b.owned(user, name); err != nil {
		return err
	}
	if err := b.wake(user, name); err != nil {
		return err
	}
	// The VM's own host key, read with the VM under the lock: a VM that
	// takes its name and address after it is deleted has another.
	b.mu.RLock()
	v, err := b.owned(user, name)
	var hostKey string
	if err == nil {
		hostKey, err = b.hv.hostKey(v.Name)
	}
	b.mu.RUnlock()
	if err != nil {
		return err
	}
	// Running a command is using the VM: it does not go to sleep meanwhile.
	b.busy(v.Name, 1)
	defer b.busy(v.Name, -1)
	out, err := b.hv.inVMWithKey(v, hostKey, execTimeout, cmd)
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 255: // ssh's own failure
		log.Printf("%s: exec in %s: %s", user, v.Name, bytes.TrimSpace(out))
		return hostErrorf("could not reach %s", v.Name)
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		w.Write(out)
		return exitStatus(exit.ExitCode())
	default:
		w.Write(out)
		log.Printf("%s: exec in %s: %v", user, v.Name, errors.Unwrap(err))
		return hostErrorf("could not run the command in %s, or it ran for more than %s", v.Name, execTimeout)
	}
	_, err = w.Write(out)
	return err
}

// api serves POST /exec on w's domain with the commands of this box.
func (b *Box) api(w *Web) {
	w.api = http.TimeoutHandler(http.HandlerFunc(b.serveAPI), apiTimeout, apiTimeoutMsg)
}

// serveAPI runs the command line in the body as the member whose token is
// in Authorization. Never with a cookie: pages of VMs, on the same site,
// could otherwise send commands with a member's browser.
func (b *Box) serveAPI(rw http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(status)
		io.WriteString(rw, msg+"\n")
	}
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		fail(http.StatusMethodNotAllowed, "POST a command line, as typed after ssh "+b.name+".")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	t, err := b.store.TokenOf(tokenHash(strings.TrimSpace(token)))
	switch {
	case !ok || err == errNotFound:
		rw.Header().Set("WWW-Authenticate", `Bearer realm="boxe"`)
		fail(http.StatusUnauthorized, "A valid token is needed: ssh "+b.name+" token new.")
		return
	case err != nil:
		log.Printf("api: %v", err)
		fail(http.StatusInternalServerError, "Could not check the token, try again.")
		return
	case !t.Expires.After(time.Now()):
		rw.Header().Set("WWW-Authenticate", `Bearer realm="boxe", error="invalid_token"`)
		fail(http.StatusUnauthorized, "This token expired: make another with ssh "+b.name+" token new.")
		return
	case !b.keys.InTeam(t.User):
		fail(http.StatusForbidden, t.User+" is no longer in the team.")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxAPILine))
	if err != nil {
		fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("The command line is longer than %dK.", maxAPILine>>10))
		return
	}
	line := strings.TrimSpace(string(raw))
	l := parseLine(line)
	if l.command == "token" {
		fail(http.StatusForbidden, "Tokens are made and deleted over SSH only: ssh "+b.name+" token.")
		return
	}
	if len(t.Cmds) > 0 && !slices.Contains(t.Cmds, l.command) {
		fail(http.StatusForbidden, "This token may only run "+strings.Join(t.Cmds, ", ")+".")
		return
	}
	if err := b.store.UsedToken(t.ID, time.Now()); err != nil {
		log.Printf("api: token %s: %v", t.ID, err)
	}
	log.Printf("api: %s (token %s): %s", t.User, t.ID, l.command)
	var out bytes.Buffer
	err = b.run(&out, noInput, t.User, line, "")
	var status exitStatus
	var host hostError
	code, exit := http.StatusOK, 0
	switch {
	case err == nil:
	case errors.As(err, &status):
		code, exit = http.StatusUnprocessableEntity, int(status)
	case errors.As(err, &host):
		code, exit = http.StatusInternalServerError, 1
	default:
		code, exit = http.StatusBadRequest, 1
	}
	if err != nil && status == 0 && !l.json {
		fmt.Fprintln(&out, "error:", err)
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if l.command == "exec" {
		rw.Header().Set("X-Exit-Status", strconv.Itoa(exit))
	}
	rw.WriteHeader(code)
	rw.Write(out.Bytes())
}
