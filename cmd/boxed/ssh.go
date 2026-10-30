package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anmitsu/go-shlex"
	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type ctxKey string

const userKey ctxKey = "boxe-user"

// Box is the SSH front end: `ssh boxe <command>` and the jump host for
// `ssh <vm>.boxe`.
type Box struct {
	store  *Store
	keys   *Keys
	hv     *Hypervisor
	mu     sync.RWMutex // serializes changes to VMs; jumps hold it for reading
	limits Limits
	name   string // what users ssh to, e.g. boxe

	backupConf, backupLast string // backups are on if backupConf exists; backupLast: when the last complete one ended
	backupStatus           string // the last run, as boxe-backup.sh writes it

	connsMu sync.Mutex
	conns   map[ssh.Context]net.Conn // open SSH connections, to drop revoked users

	sleepAfter time.Duration // an unused VM goes to sleep after this long; 0: never
	actMu      sync.Mutex
	act        map[string]*activity // running VMs: how they are used (sleep.go)
}

const keyKey ctxKey = "boxe-key"

// revalidate drops the connections whose user or key is no longer valid.
// Runs after every key refresh.
func (b *Box) revalidate() {
	b.connsMu.Lock()
	defer b.connsMu.Unlock()
	for ctx, conn := range b.conns {
		user, _ := ctx.Value(userKey).(string)
		key, _ := ctx.Value(keyKey).(string)
		if u, ok := b.keys.userOf(key); !ok || u != user {
			log.Printf("%s: key revoked, disconnecting", user)
			conn.Close()
		}
	}
}

// RefreshLoop refreshes the keys and drops revoked users, every so often.
func (b *Box) RefreshLoop(every time.Duration) {
	for {
		time.Sleep(every)
		b.keys.Refresh()
		b.revalidate()
	}
}

// Limits keep one member, or one mistake, from taking the whole host.
// Sizes in MiB; 0 means no limit.
type Limits struct {
	VMMem   int // per VM
	UserMem int // total of a member's VMs that are awake
	UserVMs int // number of a member's VMs
}

// admit says whether user may have one more VM with mem.
func (b *Box) admit(user, mem string) error {
	mib, err := memMiB(mem)
	if err != nil {
		return err
	}
	if b.limits.VMMem > 0 && mib > b.limits.VMMem {
		return fmt.Errorf("at most %dG of RAM per VM", b.limits.VMMem/1024)
	}
	vms, err := b.store.List(user)
	if err != nil {
		return err
	}
	if b.limits.UserVMs > 0 && len(vms) >= b.limits.UserVMs {
		return fmt.Errorf("you already have %d VMs, the most you can have: delete one first", len(vms))
	}
	total := mib
	for _, v := range vms {
		if b.hv.Sleeping(v.Name) {
			continue // its memory is on disk
		}
		m, _ := memMiB(v.Mem)
		total += m
	}
	if b.limits.UserMem > 0 && total > b.limits.UserMem {
		return fmt.Errorf("your awake VMs would use %dG of RAM, more than your %dG: put one to sleep (ssh %s sleep <name>), delete one or use less --mem",
			total/1024, b.limits.UserMem/1024, b.name)
	}
	return nil
}

func (b *Box) server() *ssh.Server {
	return &ssh.Server{
		Handler: b.session,
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			u, ok := b.keys.User(key)
			if ok {
				ctx.SetValue(userKey, u)
				ctx.SetValue(keyKey, string(key.Marshal()))
			}
			return ok
		},
		ConnCallback: func(ctx ssh.Context, conn net.Conn) net.Conn {
			b.connsMu.Lock()
			if b.conns == nil {
				b.conns = map[ssh.Context]net.Conn{}
			}
			b.conns[ctx] = conn
			b.connsMu.Unlock()
			go func() {
				<-ctx.Done()
				b.connsMu.Lock()
				delete(b.conns, ctx)
				b.connsMu.Unlock()
			}()
			return conn
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": b.directTCPIP,
		},
	}
}

// help is the text of `ssh boxe`, with this installation's names; cmd
// comes before each command ("ssh boxe ", or nothing at the prompt).
func (b *Box) help(cmd string) string {
	h := strings.NewReplacer("SSH ", cmd, "SUFFIX", b.hv.SSHSuffix).Replace(`boxe: microVMs for your team

  SSH new [name]                        create a VM (options: --cpu 2 --mem 2G --ttl 2h --image <name>)
  SSH ls [-a]                           your VMs (-a: everyone's)
  SSH rm <name>                         delete a VM (an admin: anyone's)
  SSH cp <name> [new-name]              copy a VM, even a running one (as after a power cut)
  SSH fork <name> [new-name]            copy a running VM with its memory: programs keep running
  SSH ttl <name> <2h|3d|off>            delete a VM by itself after a while
  SSH restart <name>                    power a VM off and on again (an admin: anyone's)
  SSH sleep <name>                      put a VM to sleep now (it wakes up when you connect)
  SSH wake <name>                       wake a VM up
  SSH keep-awake <name> on|off          never put this VM to sleep when unused
  SSH share <name> [port]               a link that shows the VM (port 8000) to anyone who has it
  SSH unshare <name>                    make the VM's links stop working
  SSH exec <name> <command>             run a command in a VM, as ssh <name>.SUFFIX <command> does
  SSH images                            the images new VMs can start from
  SSH token new [--label x] [--exp 30d] [--cmds ls,new]
                                        a token for the HTTPS API (token ls, token rm <id|label>)
  SSH ssh-config                        config for ~/.ssh/config
  SSH known-hosts                       line for ~/.ssh/known_hosts
  SSH whoami

Add --json to a command for output a program can read.
Add --agent <what to do>, last, to new, cp or fork: the VM's agent starts on it.

To log into a VM: ssh <name>.SUFFIX
`)
	if d := b.hv.WebDomain; d != "" {
		h += fmt.Sprintf("On the web: https://<name>.%s (port 8000),\n            https://<name>--<port>.%s (ports 3000-9999),\n            https://<name>--agent.%s (the agent, yours only),\n            https://<name>--desktop.%s (a desktop with a browser, yours only)\n", d, d, d, d)
	}
	return h + fmt.Sprintf("\nOnce per computer:\n  ssh %[1]s ssh-config >> ~/.ssh/config\n  ssh %[1]s known-hosts >> ~/.ssh/known_hosts\n", b.name)
}

func (b *Box) session(s ssh.Session) {
	user, _ := s.Context().Value(userKey).(string)
	if pty, win, isPty := s.Pty(); isPty && s.RawCommand() == "" {
		b.prompt(s, user, pty.Window, win)
		s.Exit(0)
		return
	}
	if err := b.run(s, user, s.RawCommand(), "ssh "+b.name+" "); err != nil {
		// A command run in a VM exits as it did there, as with ssh <vm>.
		var status exitStatus
		if errors.As(err, &status) {
			s.Exit(int(status))
			return
		}
		fmt.Fprintln(s.Stderr(), "error:", err)
		s.Exit(1)
		return
	}
	s.Exit(0)
}

// run is one command line, as typed after `ssh boxe` or at the prompt;
// cmd is how the help writes a command there.
func (b *Box) run(w io.Writer, user, line, cmd string) error {
	l := parseLine(line)
	// exec passes the rest of the line to the VM as typed: --json and
	// --agent included.
	if l.command == "exec" {
		return b.cmdExec(w, user, line)
	}
	args, agent, hasAgent := l.args, l.agent, l.hasAgent
	o := output{w: w, json: l.json}
	if len(args) == 0 {
		io.WriteString(w, b.help(cmd))
		return nil
	}
	var err error
	if hasAgent && agent == "" {
		err = fmt.Errorf("--agent needs a prompt, last on the line: ssh %s %s ... --agent <what to do>", b.name, args[0])
	} else if hasAgent && args[0] != "new" && args[0] != "cp" && args[0] != "fork" {
		err = fmt.Errorf("--agent works with new, cp and fork")
	}
	switch {
	case err != nil:
	case args[0] == "new":
		err = b.cmdNew(o, user, args[1:], agent)
	case args[0] == "ls":
		err = b.cmdLs(o, user, args[1:])
	case args[0] == "rm":
		err = b.cmdRm(o, user, args[1:])
	case args[0] == "cp":
		err = b.cmdCp(o, user, args[1:], false, agent)
	case args[0] == "fork":
		err = b.cmdCp(o, user, args[1:], true, agent)
	case args[0] == "restart":
		err = b.cmdRestart(o, user, args[1:])
	case args[0] == "ttl":
		err = b.cmdTTL(o, user, args[1:])
	case args[0] == "sleep":
		err = b.cmdSleep(o, user, args[1:])
	case args[0] == "wake":
		err = b.cmdWake(o, user, args[1:])
	case args[0] == "keep-awake":
		err = b.cmdKeepAwake(o, user, args[1:])
	case args[0] == "share":
		err = b.cmdShare(o, user, args[1:])
	case args[0] == "unshare":
		err = b.cmdUnshare(o, user, args[1:])
	case args[0] == "images":
		err = b.cmdImages(o)
	case args[0] == "token":
		err = b.cmdToken(o, user, args[1:])
	case args[0] == "ssh-config":
		fmt.Fprintf(w, "Host *.%s\n  User exedev\n  ProxyJump %s\n", b.hv.SSHSuffix, b.name)
	case args[0] == "known-hosts":
		// Every VM's host key is signed by boxe: one line trusts them all.
		fmt.Fprintf(w, "@cert-authority *.%s %s", b.hv.SSHSuffix, gossh.MarshalAuthorizedKey(b.hv.HostCA.PublicKey()))
	case args[0] == "whoami":
		o.print(map[string]string{"user": user}, user+"\n")
	case args[0] == "help":
		io.WriteString(w, b.help(cmd))
	default:
		err = fmt.Errorf("unknown command: %s (ssh %s help)", args[0], b.name)
	}
	if err != nil && l.json {
		o.print(map[string]string{"error": err.Error()}, "")
	}
	return err
}

// line is a command line, split as run reads it.
type line struct {
	command        string   // what runs it: args[0], or "help"
	args           []string // the words, without --json and --agent
	json, hasAgent bool
	agent          string
}

// parseLine splits a command line as run does. The API checks command
// before running the line: both must read it the same way.
func parseLine(s string) line {
	raw, agent, hasAgent := cutAgent(s)
	words, _ := shlex.Split(raw, true)
	args, asJSON := cutFlag(words, "--json")
	l := line{command: "help", args: args, json: asJSON, hasAgent: hasAgent, agent: agent}
	if len(args) > 0 {
		l.command = args[0]
	}
	return l
}

// hostError is a command that failed on the host's side, not because of
// what was asked: the API answers it with a 500.
type hostError struct{ error }

func hostErrorf(format string, a ...any) error { return hostError{fmt.Errorf(format, a...)} }

// output prints a command's result for people, or as JSON with --json.
type output struct {
	w    io.Writer
	json bool
}

func (o output) print(v any, text string) {
	if o.json {
		json.NewEncoder(o.w).Encode(v)
		return
	}
	io.WriteString(o.w, text)
}

// cutAgent splits the command line at --agent: what follows, as typed,
// is the prompt for the VM's agent. Taken before the line is split into
// words, so that quotes and apostrophes in the prompt need no escaping.
func cutAgent(raw string) (rest, prompt string, found bool) {
	for i := 0; i+len("--agent") <= len(raw); i++ {
		end := i + len("--agent")
		if raw[i:end] == "--agent" && (i == 0 || raw[i-1] == ' ') && (end == len(raw) || raw[end] == ' ') {
			return raw[:i], strings.TrimSpace(raw[end:]), true
		}
	}
	return raw, "", false
}

// cutFlag removes flag from args, wherever it is, and says whether it was there.
func cutFlag(args []string, flag string) ([]string, bool) {
	var rest []string
	found := false
	for _, a := range args {
		if a == flag {
			found = true
		} else {
			rest = append(rest, a)
		}
	}
	return rest, found
}

// vmJSON is a VM as `--json` shows it.
type vmJSON struct {
	Name         string  `json:"name"`
	Owner        string  `json:"owner"`
	IP           string  `json:"ip"`
	CPUs         int     `json:"cpus"`
	Mem          string  `json:"mem"`
	State        string  `json:"state"`
	Created      string  `json:"created"`
	Image        string  `json:"image"`
	SSH          string  `json:"ssh"`
	URL          string  `json:"url,omitempty"`
	ReadySeconds float64 `json:"ready_seconds,omitempty"`
	Expires      string  `json:"expires,omitempty"` // when it is deleted by itself (--ttl)
	KeepAwake    bool    `json:"keep_awake,omitempty"`
	Agent        string  `json:"agent,omitempty"` // the conversation --agent started
	// What it uses, in ls: CPU over the last minute (100: one core), the
	// RAM it takes on the host and the space its disk takes, in bytes.
	CPUPercent float64 `json:"cpu_percent,omitempty"`
	MemUsed    int64   `json:"mem_used,omitempty"`
	DiskUsed   int64   `json:"disk_used,omitempty"`
}

func (b *Box) vmJSON(v VM) vmJSON {
	j := vmJSON{Name: v.Name, Owner: v.Owner, IP: v.IP(), CPUs: v.CPUs, Mem: v.Mem, State: b.hv.State(v.Name),
		Created: v.Created.Format(time.RFC3339), Image: imageName(v.Image), SSH: v.Name + "." + b.hv.SSHSuffix}
	if d := b.hv.WebDomain; d != "" {
		j.URL = "https://" + v.Name + "." + d
	}
	j.Expires = timeCol(v.Expires)
	j.KeepAwake = v.KeepAwake
	return j
}

// imageName is the short name of an image snapshot, e.g. boxeuntu-20261029.
func imageName(snapshot string) string {
	return strings.TrimSuffix(snapshot[strings.LastIndex(snapshot, "/")+1:], "@base")
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

func checkName(name string) error {
	// "--" separates the port in https://<vm>--<port>.<domain>.
	if !validName.MatchString(name) || name == "boxe" || strings.Contains(name, "--") {
		return fmt.Errorf("invalid name %q: lowercase letters, digits and single dashes, 2 to 32 characters", name)
	}
	return nil
}

var (
	adjectives = []string{"agile", "bold", "brave", "bright", "calm", "clever", "eager", "gentle", "happy", "keen", "lucky", "nimble", "quick", "quiet", "steady", "swift"}
	nouns      = []string{"badger", "beaver", "deer", "dolphin", "eagle", "falcon", "fox", "heron", "lynx", "marmot", "otter", "owl", "panda", "sparrow", "squirrel", "wolf"}
)

func (b *Box) randomName() (string, error) {
	for range 200 {
		n := adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))]
		_, err := b.store.Get(n)
		if err == errNotFound {
			return n, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free random name: choose one")
}

// parseVMFlags parses [name] [--cpu N] [--mem SIZE] in any order.
func parseVMFlags(args []string) (name string, cpus int, mem string, err error) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&cpus, "cpu", 2, "")
	fs.StringVar(&mem, "mem", "2G", "")
	var pos []string
	for len(args) > 0 {
		if err = fs.Parse(args); err != nil {
			return
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	if len(pos) > 1 {
		err = fmt.Errorf("too many arguments: %s", strings.Join(pos, " "))
	} else if len(pos) == 1 {
		name = pos[0]
	}
	if cpus < 1 || cpus > 8 {
		err = fmt.Errorf("--cpu between 1 and 8")
	}
	if _, merr := memMiB(mem); merr != nil {
		err = fmt.Errorf("--mem like 512M or 4G")
	}
	return
}

func (b *Box) cmdNew(o output, user string, args []string, agent string) error {
	args, ttl, err := cutTTL(args)
	if err != nil {
		return err
	}
	args, image, err := cutImage(args)
	if err != nil {
		return err
	}
	name, cpus, mem, err := parseVMFlags(args)
	if err != nil {
		return err
	}
	snapshot := b.hv.BaseSnapshot
	if image != "" {
		if snapshot, err = b.hv.Image(image); err != nil {
			return fmt.Errorf("%v: ssh %s images lists them", err, b.name)
		}
	}
	return b.create(o, user, name, cpus, mem, "", snapshot, ttl, false, agent)
}

// cutImage takes --image <name> out of args.
func cutImage(args []string) ([]string, string, error) {
	for i, a := range args {
		if a == "--image" || a == "-image" {
			if i+1 == len(args) {
				return nil, "", fmt.Errorf("--image needs a name: ssh boxe images lists them")
			}
			return append(args[:i:i], args[i+2:]...), args[i+1], nil
		}
	}
	return args, "", nil
}

func (b *Box) cmdImages(o output) error {
	images, err := b.hv.Images()
	if err != nil {
		log.Printf("images: %v", err)
		return hostErrorf("could not list the images")
	}
	if o.json {
		o.print(images, "")
		return nil
	}
	w := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\t")
	for _, im := range images {
		def := ""
		if im.Default {
			def = "default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", im.Name, im.Version, def)
	}
	fmt.Fprintf(w, "\nssh %s new --image <name> starts from the newest version.\n", b.name)
	return w.Flush()
}

// shareJSON is the answer of share --json.
type shareJSON struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	URL  string `json:"url"`
}

func (b *Box) cmdShare(o output, user string, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: ssh %s share <name> [port]", b.name)
	}
	if b.hv.WebDomain == "" {
		return fmt.Errorf("this installation has no web front end")
	}
	port := vmWebPort
	if len(args) == 2 {
		p, err := strconv.Atoi(args[1])
		if err != nil || p < minVMPort || p > 9999 {
			return fmt.Errorf("invalid port %q: 3000 to 9999", args[1])
		}
		port = p
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	token, err := b.store.Share(v, port)
	if err != nil {
		return err
	}
	host := v.Name
	if port != vmWebPort {
		host += fmt.Sprintf("--%d", port)
	}
	j := shareJSON{v.Name, port, "https://" + host + "." + b.hv.WebDomain + "/__boxe/share/" + token}
	log.Printf("%s: shared port %d of %s", user, port, v.Name)
	o.print(j, fmt.Sprintf("Anyone with this link sees port %d of %s, without logging in (ssh %s unshare %s to stop it):\n\n  %s\n",
		port, v.Name, b.name, v.Name, j.URL))
	return nil
}

func (b *Box) cmdUnshare(o output, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s unshare <name>", b.name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	n, err := b.store.Unshare(v.Name)
	if err != nil {
		return err
	}
	log.Printf("%s: unshared %s (%d links)", user, v.Name, n)
	o.print(done{v.Name, "unshared"}, fmt.Sprintf("%d links of %s no longer work\n", n, v.Name))
	return nil
}

// cutTTL takes --ttl <duration> out of args.
func cutTTL(args []string) ([]string, time.Duration, error) {
	for i, a := range args {
		if a == "--ttl" || a == "-ttl" {
			if i+1 == len(args) {
				return nil, 0, fmt.Errorf("--ttl needs a duration, like 2h or 3d")
			}
			ttl, err := parseTTL(args[i+1])
			return append(args[:i:i], args[i+2:]...), ttl, err
		}
	}
	return args, 0, nil
}

const maxTTL = 30 * 24 * time.Hour

// parseTTL reads durations like 30m, 2h or 3d.
func parseTTL(s string) (time.Duration, error) {
	d, err := parseDays(s)
	if err != nil || d < time.Minute || d > maxTTL {
		return 0, fmt.Errorf("invalid ttl %q: a duration like 30m, 2h or 3d, at most 30d", s)
	}
	return d, nil
}

// parseDays reads a duration, also in days: 30m, 2h, 3d.
func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		return time.Duration(days) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

// cmdCp copies a VM: its disk, or with fork its memory too.
func (b *Box) cmdCp(o output, user string, args []string, fork bool, agent string) error {
	args, ttl, err := cutTTL(args)
	if err != nil {
		return err
	}
	if len(args) < 1 || len(args) > 2 {
		cmd := "cp"
		if fork {
			cmd = "fork"
		}
		return fmt.Errorf("usage: ssh %s %s <name> [new-name]", b.name, cmd)
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	if fork {
		// Only a running VM can be forked.
		if _, err := b.owned(user, args[0]); err != nil {
			return err
		}
		if err := b.wake(user, args[0]); err != nil {
			return err
		}
	}
	return b.create(o, user, name, 0, "", args[0], "", ttl, fork, agent)
}

// create makes a new VM from the image snapshot, or a copy of the VM named
// src (then cpus, mem and image come from it). The source is looked up under the lock: it cannot be
// replaced by another VM with the same name meanwhile. With a ttl, the VM
// deletes itself after that long. With agent, a prompt, the VM's agent
// starts working on it.
func (b *Box) create(o output, user, name string, cpus int, mem string, srcName, image string, ttl time.Duration, fork bool, agent string) error {
	start := time.Now()
	b.mu.Lock()
	var src *VM
	if srcName != "" {
		v, err := b.owned(user, srcName)
		if err != nil {
			b.mu.Unlock()
			return err
		}
		src, cpus, mem, image = &v, v.CPUs, v.Mem, v.Image
	}
	if name == "" {
		var err error
		if name, err = b.randomName(); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	if err := checkName(name); err != nil {
		b.mu.Unlock()
		return err
	}
	if _, err := b.store.Get(name); err == nil {
		b.mu.Unlock()
		return fmt.Errorf("a VM named %s already exists", name)
	}
	if err := b.admit(user, mem); err != nil {
		b.mu.Unlock()
		return err
	}
	if b.hv.MinFree > 0 {
		if free, err := b.hv.FreeSpace(); err != nil || free < b.hv.MinFree {
			b.mu.Unlock()
			log.Printf("%s: create %s: free space %d: %v", user, name, free, err)
			return hostErrorf("the host is short of disk space: ask an admin")
		}
	}
	v := VM{Name: name, Owner: user, CPUs: cpus, Mem: mem, Image: image}
	if ttl > 0 {
		v.Expires = start.Add(ttl)
	}
	if err := b.store.Insert(&v); err != nil {
		b.mu.Unlock()
		return err
	}
	var err error
	if fork {
		err = b.hv.Fork(*src, v, b.keys.AuthorizedKeys(user))
	} else if src != nil {
		err = b.hv.Copy(*src, v, b.keys.AuthorizedKeys(user))
	} else {
		err = b.hv.Create(v, image, b.keys.AuthorizedKeys(user))
	}
	if err != nil {
		log.Printf("%s: create %s: %v", user, name, err)
		if errors.Is(err, errLeftBehind) {
			b.mu.Unlock()
			return hostErrorf("could not create the VM, nor clean up after it: delete it with ssh %s rm %s", b.name, name)
		}
		b.store.Delete(name)
		b.mu.Unlock()
		if strings.Contains(err.Error(), "could not flush") {
			return hostErrorf("could not copy: %s did not flush its disk; try again", srcName)
		}
		if errors.Is(err, errNoAgent) {
			return fmt.Errorf("%s comes from an older image and cannot be forked: copy it with cp", srcName)
		}
		return hostErrorf("could not create the VM")
	}
	b.mu.Unlock()
	if err := b.hv.WaitSSH(v, 30*time.Second); err != nil {
		return err
	}
	took := time.Since(start)
	log.Printf("%s: created %s (%s) in %s", user, name, v.IP(), took.Round(time.Millisecond))
	j := b.vmJSON(v)
	j.ReadySeconds = took.Round(time.Millisecond).Seconds()
	text := fmt.Sprintf("%s ready in %.1f s\n\n  ssh %s.%s\n\n  IP %s, %d CPU, %s RAM\n",
		name, took.Seconds(), name, b.hv.SSHSuffix, v.IP(), v.CPUs, v.Mem)
	if !v.Expires.IsZero() {
		text += fmt.Sprintf("  Deleted by itself %s (ssh %s ttl %s off to keep it)\n", expiresIn(v.Expires, time.Now()), b.name, name)
	}
	if agent != "" {
		// Under the lock, so that v cannot be deleted and its name and
		// address go to someone else's VM while the prompt is on its way.
		b.mu.Lock()
		cur, err := b.store.Get(name)
		if err != nil || !cur.same(v) {
			b.mu.Unlock()
			return fmt.Errorf("%s was deleted before its agent could start", name)
		}
		slug, err := b.hv.StartAgent(v, agent)
		b.mu.Unlock()
		if err != nil {
			log.Printf("%s: agent in %s: %v", user, name, err)
			return fmt.Errorf("%s is ready, but %v", name, err)
		}
		j.Agent = fmt.Sprintf("https://%s--agent.%s/c/%s", name, b.hv.WebDomain, slug)
		text += "\n  Agent at work: " + j.Agent + "\n"
	}
	o.print(j, text)
	return nil
}

func (b *Box) owned(user, name string) (VM, error) {
	v, err := b.store.Get(name)
	if err == errNotFound || (err == nil && v.Owner != user) {
		return v, fmt.Errorf("you have no VM named %s", name)
	}
	return v, err
}

func (b *Box) cmdRestart(o output, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s restart <name>", b.name)
	}
	v, err := b.restart(user, b.keys.Admin(user), args[0])
	if err != nil {
		return err
	}
	if err := b.hv.WaitSSH(v, 30*time.Second); err != nil {
		return err
	}
	o.print(ownedDone{v.Name, "restarted", v.Owner}, v.Name+" restarted"+whose(user, v, "is")+"\n")
	return nil
}

// whose tells an admin that the VM they changed is (or was) someone else's.
func whose(user string, v VM, verb string) string {
	if v.Owner == user {
		return ""
	}
	return " (it " + verb + " " + v.Owner + "'s)"
}

// manageable is the VM name if user may restart or delete it: theirs, or
// anyone's for an admin.
func (b *Box) manageable(user string, admin bool, name string) (VM, error) {
	v, err := b.owned(user, name)
	if err != nil && admin {
		if v, err := b.store.Get(name); err == nil {
			return v, nil
		}
	}
	return v, err
}

// restart stops and starts the VM name of user (any VM for an admin).
func (b *Box) restart(user string, admin bool, name string) (VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.manageable(user, admin, name)
	if err != nil {
		return v, err
	}
	if b.hv.Sleeping(v.Name) {
		if err := b.makeRoom(v); err != nil {
			return v, err
		}
	}
	if err = b.hv.Stop(v); err == nil {
		err = b.hv.Start(v)
	}
	if err != nil {
		log.Printf("%s: restart %s: %v", user, v.Name, err)
		return v, hostErrorf("could not restart %s", v.Name)
	}
	log.Printf("%s: restarted %s%s", user, v.Name, whose(user, v, "is"))
	return v, nil
}

func (b *Box) cmdTTL(o output, user string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: ssh %s ttl <name> <2h|3d|off>", b.name)
	}
	var exp time.Time
	if args[1] != "off" {
		ttl, err := parseTTL(args[1])
		if err != nil {
			return err
		}
		exp = time.Now().Add(ttl)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	if err := b.store.SetExpires(v.Name, exp); err != nil {
		return err
	}
	v.Expires = exp
	text := v.Name + " is deleted " + expiresIn(exp, time.Now()) + "\n"
	if exp.IsZero() {
		text = v.Name + " stays until you delete it\n"
	}
	o.print(b.vmJSON(v), text)
	return nil
}

// expiresIn says when a VM with a ttl is deleted, e.g. "in 1h59m".
func expiresIn(exp, now time.Time) string {
	if exp.IsZero() {
		return "-"
	}
	d := exp.Sub(now).Round(time.Minute)
	if d <= 0 {
		return "now"
	}
	return "in " + strings.TrimSuffix(d.String(), "0s")
}

// reap deletes the VMs whose ttl ran out by now.
func (b *Box) reap(now time.Time) {
	vms, err := b.store.List("")
	if err != nil {
		log.Printf("reap: %v", err)
		return
	}
	for _, v := range vms {
		if v.Expires.IsZero() || v.Expires.After(now) {
			continue
		}
		b.mu.Lock()
		// The ttl may have changed meanwhile, or the VM been replaced.
		if cur, err := b.store.Get(v.Name); err == nil && cur.Index == v.Index && !cur.Expires.IsZero() && !cur.Expires.After(now) {
			if err := b.hv.Destroy(cur); err != nil {
				log.Printf("%s: expired %s: %v", cur.Owner, cur.Name, err)
			} else if err := b.store.Delete(cur.Name); err != nil {
				log.Printf("%s: expired %s: %v", cur.Owner, cur.Name, err)
			} else {
				log.Printf("%s: removed %s, its ttl ran out", cur.Owner, cur.Name)
			}
		}
		b.mu.Unlock()
	}
}

// ReapLoop deletes expired VMs and old images, every so often.
func (b *Box) ReapLoop(every time.Duration) {
	for {
		b.reap(time.Now())
		b.pruneImages()
		time.Sleep(every)
	}
}

func (b *Box) cmdRm(o output, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s rm <name>", b.name)
	}
	v, err := b.remove(user, b.keys.Admin(user), args[0])
	if err != nil {
		return err
	}
	o.print(ownedDone{v.Name, "deleted", v.Owner}, v.Name+" deleted"+whose(user, v, "was")+"\n")
	return nil
}

// remove deletes the VM name of user (any VM for an admin) and its disk.
func (b *Box) remove(user string, admin bool, name string) (VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.manageable(user, admin, name)
	if err != nil {
		return v, err
	}
	if err := b.hv.Destroy(v); err != nil {
		log.Printf("%s: rm %s: %v", user, v.Name, err)
		return v, hostErrorf("could not delete the VM")
	}
	log.Printf("%s: removed %s%s", user, v.Name, whose(user, v, "was"))
	return v, b.store.Delete(v.Name)
}

// done is the --json answer of commands that change a VM.
type done struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ownedDone is done with the VM's owner: an admin's rm and restart may be
// of someone else's.
type ownedDone struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Owner  string `json:"owner"`
}

func (b *Box) cmdLs(o output, user string, args []string) error {
	owner := user
	if len(args) == 1 && args[0] == "-a" {
		owner = ""
	}
	now := time.Now()
	vms, list, err := b.list(owner, now)
	if err != nil {
		return err
	}
	if o.json {
		o.print(list, "")
		return nil
	}
	w := tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tIP\tCPU\tRAM\tSTATE\tCPU%\tRAM USED\tDISK USED\tOWNER\tCREATED\tEXPIRES\tIMAGE")
	for i, j := range list {
		image := j.Image
		if image == "" {
			image = "-"
		}
		cpu := "-"
		if j.CPUPercent > 0 {
			cpu = fmt.Sprintf("%.0f%%", j.CPUPercent)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.Name, j.IP, j.CPUs, j.Mem, j.State,
			cpu, size(j.MemUsed), size(j.DiskUsed), j.Owner,
			vms[i].Created.Local().Format("2006-01-02 15:04"), expiresIn(vms[i].Expires, now), image)
	}
	return w.Flush()
}

// list is the VMs of owner (all with ""), with what each uses, as ls and
// the panel show them.
func (b *Box) list(owner string, now time.Time) ([]VM, []vmJSON, error) {
	vms, err := b.store.List(owner)
	if err != nil {
		return nil, nil, err
	}
	list := []vmJSON{}
	disks := b.hv.DiskUsage()
	for _, v := range vms {
		j := b.vmJSON(v)
		if j.State == "running" {
			j.CPUPercent = b.cpuPercent(v.Name, now)
			j.MemUsed = b.hv.MemUsage(v.Name)
		}
		j.DiskUsed = disks[v.Name]
		list = append(list, j)
	}
	return vms, list, nil
}

// size is a number of bytes for people, e.g. 700M or 5.1G; "-" for none.
func size(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1<<30:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}

// directTCPIP makes boxe a jump host: `ssh -J boxe test.boxe` and port
// forwards like `ssh -L 8000:test.boxe:8000 boxe` reach the user's own VMs.
func (b *Box) directTCPIP(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	var d struct {
		DestAddr string
		DestPort uint32
		OrigAddr string
		OrigPort uint32
	}
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, "invalid request")
		return
	}
	user, _ := ctx.Value(userKey).(string)
	name := strings.TrimSuffix(d.DestAddr, "."+b.hv.SSHSuffix)
	if err := b.wake(user, name); err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	// Look up and connect under the lock: the VM cannot be deleted and its
	// address given to someone else's VM in between.
	b.mu.RLock()
	v, err := b.owned(user, name)
	if err != nil {
		b.mu.RUnlock()
		newChan.Reject(gossh.Prohibited, err.Error())
		return
	}
	dst, err := b.hv.dial("tcp", net.JoinHostPort(v.IP(), strconv.Itoa(int(d.DestPort))), 3*time.Second)
	b.mu.RUnlock()
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	// An open connection keeps the VM awake.
	b.busy(v.Name, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		wg.Wait()
		b.busy(v.Name, -1)
	}()
	go func() {
		defer wg.Done()
		defer ch.Close()
		defer dst.Close()
		io.Copy(ch, dst)
	}()
	go func() {
		defer wg.Done()
		defer ch.Close()
		defer dst.Close()
		io.Copy(dst, ch)
	}()
}

// problems lists what needs an admin, for /healthz: no names or details,
// the endpoint is public.
func (b *Box) problems() []string {
	var p []string
	vms, err := b.store.List("")
	if err != nil {
		p = append(p, "database not readable")
	}
	failed := 0
	for _, v := range vms {
		if b.hv.State(v.Name) == "failed" {
			failed++
		}
	}
	if failed > 0 {
		p = append(p, fmt.Sprintf("%d VM failed", failed))
	}
	if b.hv.MinFree > 0 {
		if free, err := b.hv.FreeSpace(); err != nil || free < b.hv.MinFree {
			p = append(p, "disk space low")
		}
	}
	if since := time.Since(b.keys.LastOK()); since > 24*time.Hour {
		p = append(p, fmt.Sprintf("GitHub keys not refreshed for %s", since.Round(time.Hour)))
	}
	if _, err := os.Stat(b.backupConf); err == nil {
		raw, _ := os.ReadFile(b.backupLast)
		last, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if age := time.Since(time.Unix(last, 0)); err != nil || age > 36*time.Hour {
			p = append(p, "backup older than 36h")
		}
	}
	return p
}
