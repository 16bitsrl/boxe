package main

import (
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

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type ctxKey string

const userKey ctxKey = "box-user"

// Box is the SSH front end: `ssh box <command>` and the jump host for
// `ssh <vm>.box`.
type Box struct {
	store  *Store
	keys   *Keys
	hv     *Hypervisor
	mu     sync.RWMutex // serializes changes to VMs; jumps hold it for reading
	limits Limits
	name   string // what users ssh to, e.g. box

	backupConf, backupLast string // backups are on if backupConf exists; backupLast: when the last one ended

	connsMu sync.Mutex
	conns   map[ssh.Context]net.Conn // open SSH connections, to drop revoked users
}

const keyKey ctxKey = "box-key"

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

// Limits keep one member, or one mistake, from taking the whole exe-host.
// Sizes in MiB; 0 means no limit.
type Limits struct {
	VMMem   int // per VM
	UserMem int // total of a member's VMs
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
		m, _ := memMiB(v.Mem)
		total += m
	}
	if b.limits.UserMem > 0 && total > b.limits.UserMem {
		return fmt.Errorf("your VMs would use %dG of RAM, more than your %dG: delete one or use less --mem", total/1024, b.limits.UserMem/1024)
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

// help is the text of `ssh box`, with this installation's names.
func (b *Box) help() string {
	h := strings.NewReplacer("SSH", "ssh "+b.name, "SUFFIX", b.hv.SSHSuffix).Replace(`box: microVMs for your team

  SSH new [name] [--cpu N] [--mem 2G]   create a VM
  SSH ls [-a]                           your VMs (-a: everyone's)
  SSH rm <name>                         delete a VM
  SSH cp <name> [new-name]              copy a VM, even a running one (as after a power cut)
  SSH restart <name>                    power a VM off and on again
  SSH ssh-config                        config for ~/.ssh/config
  SSH known-hosts                       line for ~/.ssh/known_hosts
  SSH whoami

To log into a VM: ssh <name>.SUFFIX
`)
	if d := b.hv.WebDomain; d != "" {
		h += fmt.Sprintf("On the web: https://<name>.%s (port 8000),\n            https://<name>--<port>.%s (ports 3000-9999)\n", d, d)
	}
	return h + fmt.Sprintf("\nOnce per computer:\n  ssh %[1]s ssh-config >> ~/.ssh/config\n  ssh %[1]s known-hosts >> ~/.ssh/known_hosts\n", b.name)
}

func (b *Box) session(s ssh.Session) {
	user, _ := s.Context().Value(userKey).(string)
	args := s.Command()
	out := s.Stderr()
	if len(args) == 0 {
		io.WriteString(s, b.help())
		s.Exit(0)
		return
	}
	var err error
	switch args[0] {
	case "new":
		err = b.cmdNew(s, user, args[1:])
	case "ls":
		err = b.cmdLs(s, user, args[1:])
	case "rm":
		err = b.cmdRm(s, user, args[1:])
	case "cp":
		err = b.cmdCp(s, user, args[1:])
	case "restart":
		err = b.cmdRestart(s, user, args[1:])
	case "ssh-config":
		fmt.Fprintf(s, "Host *.%s\n  User exedev\n  ProxyJump %s\n", b.hv.SSHSuffix, b.name)
	case "known-hosts":
		// Every VM's host key is signed by box: one line trusts them all.
		fmt.Fprintf(s, "@cert-authority *.%s %s", b.hv.SSHSuffix, gossh.MarshalAuthorizedKey(b.hv.HostCA.PublicKey()))
	case "whoami":
		fmt.Fprintln(s, user)
	case "help":
		io.WriteString(s, b.help())
	default:
		err = fmt.Errorf("unknown command: %s (ssh %s help)", args[0], b.name)
	}
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		s.Exit(1)
		return
	}
	s.Exit(0)
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

func checkName(name string) error {
	// "--" separates the port in https://<vm>--<port>.<domain>.
	if !validName.MatchString(name) || name == "box" || strings.Contains(name, "--") {
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

func (b *Box) cmdNew(s ssh.Session, user string, args []string) error {
	name, cpus, mem, err := parseVMFlags(args)
	if err != nil {
		return err
	}
	return b.create(s, user, name, cpus, mem, "")
}

func (b *Box) cmdCp(s ssh.Session, user string, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: ssh %s cp <name> [new-name]", b.name)
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	return b.create(s, user, name, 0, "", args[0])
}

// create makes a new VM, or a copy of the VM named src (then cpus and mem
// come from it). The source is looked up under the lock: it cannot be
// replaced by another VM with the same name meanwhile.
func (b *Box) create(s ssh.Session, user, name string, cpus int, mem string, srcName string) error {
	start := time.Now()
	b.mu.Lock()
	var src *VM
	image := b.hv.BaseSnapshot
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
			return fmt.Errorf("exe-host is short of disk space: ask an admin")
		}
	}
	v := VM{Name: name, Owner: user, CPUs: cpus, Mem: mem, Image: image}
	if err := b.store.Insert(&v); err != nil {
		b.mu.Unlock()
		return err
	}
	var err error
	if src != nil {
		err = b.hv.Copy(*src, v, b.keys.AuthorizedKeys(user))
	} else {
		err = b.hv.Create(v, b.hv.BaseSnapshot, b.keys.AuthorizedKeys(user))
	}
	if err != nil {
		log.Printf("%s: create %s: %v", user, name, err)
		if errors.Is(err, errLeftBehind) {
			b.mu.Unlock()
			return fmt.Errorf("could not create the VM, nor clean up after it: delete it with ssh %s rm %s", b.name, name)
		}
		b.store.Delete(name)
		b.mu.Unlock()
		if strings.Contains(err.Error(), "could not flush") {
			return fmt.Errorf("could not copy: %s did not flush its disk; try again", srcName)
		}
		return fmt.Errorf("could not create the VM")
	}
	b.mu.Unlock()
	if err := b.hv.WaitSSH(v, 30*time.Second); err != nil {
		return err
	}
	log.Printf("%s: created %s (%s) in %s", user, name, v.IP(), time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(s, "%s ready in %.1f s\n\n  ssh %s.%s\n\n  IP %s, %d CPU, %s RAM\n",
		name, time.Since(start).Seconds(), name, b.hv.SSHSuffix, v.IP(), v.CPUs, v.Mem)
	return nil
}

func (b *Box) owned(user, name string) (VM, error) {
	v, err := b.store.Get(name)
	if err == errNotFound || (err == nil && v.Owner != user) {
		return v, fmt.Errorf("you have no VM named %s", name)
	}
	return v, err
}

func (b *Box) cmdRestart(s ssh.Session, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s restart <name>", b.name)
	}
	b.mu.Lock()
	v, err := b.owned(user, args[0])
	if err == nil {
		if err = b.hv.Stop(v); err == nil {
			err = b.hv.Start(v)
		}
	}
	b.mu.Unlock()
	if err != nil {
		log.Printf("%s: restart %s: %v", user, args[0], err)
		return fmt.Errorf("could not restart %s", args[0])
	}
	if err := b.hv.WaitSSH(v, 30*time.Second); err != nil {
		return err
	}
	fmt.Fprintf(s, "%s restarted\n", v.Name)
	return nil
}

func (b *Box) cmdRm(s ssh.Session, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s rm <name>", b.name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	if err := b.hv.Destroy(v); err != nil {
		log.Printf("%s: rm %s: %v", user, v.Name, err)
		return fmt.Errorf("could not delete the VM")
	}
	log.Printf("%s: removed %s", user, v.Name)
	fmt.Fprintf(s, "%s deleted\n", v.Name)
	return b.store.Delete(v.Name)
}

func (b *Box) cmdLs(s ssh.Session, user string, args []string) error {
	owner := user
	if len(args) == 1 && args[0] == "-a" {
		owner = ""
	}
	vms, err := b.store.List(owner)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(s, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tIP\tCPU\tRAM\tSTATE\tOWNER\tCREATED\tIMAGE")
	for _, v := range vms {
		state := b.hv.State(v.Name)
		image := strings.TrimSuffix(v.Image[strings.LastIndex(v.Image, "/")+1:], "@base")
		if image == "" {
			image = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.IP(), v.CPUs, v.Mem, state, v.Owner, v.Created.Local().Format("2006-01-02 15:04"), image)
	}
	return w.Flush()
}

// directTCPIP makes box a jump host: `ssh -J box test.box` and port
// forwards like `ssh -L 8000:test.box:8000 box` reach the user's own VMs.
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
	// Look up and connect under the lock: the VM cannot be deleted and its
	// address given to someone else's VM in between.
	b.mu.RLock()
	v, err := b.owned(user, strings.TrimSuffix(d.DestAddr, "."+b.hv.SSHSuffix))
	if err != nil {
		b.mu.RUnlock()
		newChan.Reject(gossh.Prohibited, err.Error())
		return
	}
	dst, err := net.DialTimeout("tcp", net.JoinHostPort(v.IP(), strconv.Itoa(int(d.DestPort))), 3*time.Second)
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
	go func() {
		defer ch.Close()
		defer dst.Close()
		io.Copy(ch, dst)
	}()
	go func() {
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
