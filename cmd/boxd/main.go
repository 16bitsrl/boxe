// boxd manages the microVMs of an exe-host: an SSH server for commands
// (`ssh box new`) and a jump host to the VMs (`ssh <vm>.box`).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
	"tailscale.com/tsnet"
)

// version is set at build time by scripts/deploy.sh.
var version = "dev"

func main() {
	var (
		stateDir    = flag.String("state", "/var/lib/exe", "state directory (database, host key, tsnet)")
		usersFile   = flag.String("users", "/etc/exe/users", "GitHub usernames of the team, one per line")
		hostKey     = flag.String("vm-key", "/etc/exe/id_ed25519.pub", "exe-host's public key, added to every VM")
		base        = flag.String("base", "exe/images/boxuntu@base", "snapshot new VMs are cloned from")
		dataset     = flag.String("dataset", "exe/vms", "ZFS dataset holding the VM disks")
		kernel      = flag.String("kernel", "/var/lib/exe/kernels/vmlinux-7.2.8-exe", "guest kernel")
		tsName      = flag.String("ts-hostname", "box", "Tailscale node name, what users ssh to; empty to disable Tailscale")
		listen      = flag.String("listen", "", "extra plain TCP address to listen on, e.g. 127.0.0.1:2222")
		httpAddr    = flag.String("http", ":8080", "HTTP address for the web front end (TLS ends at the proxy in front); empty to disable")
		domain      = flag.String("domain", "", "the VMs are https://<vm>.<domain>; empty: no web front end")
		issuer      = flag.String("oidc-issuer", "", "OIDC issuer for the web login")
		org         = flag.String("oidc-org", "", "Zitadel organization whose users may log in")
		emailDoms   = flag.String("oidc-email-domains", "", "without -oidc-org: comma-separated domains of the verified emails that may log in")
		sshSuffix   = flag.String("ssh-suffix", "box", "VMs are <vm>.<suffix> for SSH")
		dns         = flag.String("dns", "1.1.1.1,1.0.0.1", "comma-separated DNS servers for the VMs")
		oidcFile    = flag.String("oidc", "/etc/exe/oidc", "file with OIDC_CLIENT_ID= and OIDC_CLIENT_SECRET=")
		showVersion = flag.Bool("version", false, "print the version and exit")
		dumpDB      = flag.String("dump-db", "", "write a consistent copy of the database to this file (for backups), then exit")
		revoke      = flag.String("revoke", "", "end every web session of this email (when someone leaves the team), then exit")
		vmUser      = flag.String("vm-user", "exe-vm", "unprivileged user the hypervisors run as")
		vmMem       = flag.String("max-vm-mem", "8G", "most RAM a VM may have")
		userMem     = flag.String("max-user-mem", "16G", "most RAM all of a member's VMs may have together")
		userVMs     = flag.Int("max-user-vms", 10, "most VMs a member may have")
		minFree     = flag.Int64("min-free-gb", 20, "refuse new VMs when the pool has less free space (GB)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetFlags(0)

	store, err := openStore(*stateDir + "/box.db")
	if err != nil {
		log.Fatal(err)
	}
	if *dumpDB != "" {
		if err := store.Dump(*dumpDB); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *revoke != "" {
		if err := store.Logout(*revoke, time.Now()); err != nil {
			log.Fatal(err)
		}
		log.Printf("web sessions of %s ended", *revoke)
		return
	}
	vmKey, err := os.ReadFile(*hostKey)
	if err != nil {
		log.Fatal(err)
	}
	keys := &Keys{usersFile: *usersFile, store: store}
	keys.Refresh()

	hostCA, err := loadHostKey(*stateDir + "/host_ca_ed25519")
	if err != nil {
		log.Fatal(err)
	}
	u, err := user.Lookup(*vmUser)
	if err != nil {
		log.Fatalf("-vm-user: %v (create it: useradd --system --no-create-home --shell /usr/sbin/nologin %s)", err, *vmUser)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	var limits Limits
	for _, l := range []struct {
		flag string
		to   *int
	}{{*vmMem, &limits.VMMem}, {*userMem, &limits.UserMem}} {
		if *l.to, err = memMiB(l.flag); err != nil {
			log.Fatal(err)
		}
	}
	limits.UserVMs = *userVMs
	name := *tsName
	if name == "" {
		name = "box"
	}
	webDomain := *domain
	if *httpAddr == "" {
		webDomain = ""
	}
	b := &Box{store: store, keys: keys, limits: limits, name: name,
		backupConf: "/etc/exe/backup.conf", backupLast: *stateDir + "/backup-last", hv: &Hypervisor{
			VMUser: *vmUser, VMUserID: uid, VMGroupID: gid, MinFree: *minFree << 30,
			BaseSnapshot: *base, Dataset: *dataset, Kernel: *kernel,
			HostPubKey: string(vmKey), HostKeyPath: strings.TrimSuffix(*hostKey, ".pub"),
			ConfigDir: *stateDir + "/config", HostCA: hostCA,
			SSHSuffix: *sshSuffix, WebDomain: webDomain, DNS: splitList(*dns),
		}}

	go b.RefreshLoop(10 * time.Minute)

	// Bring back VMs after an exe-host reboot.
	vms, err := store.List("")
	if err != nil {
		log.Fatal(err)
	}
	for _, v := range vms {
		if err := b.hv.Start(v); err != nil {
			log.Printf("start %s: %v", v.Name, err)
		}
	}

	if webDomain != "" {
		if *org == "" && *emailDoms == "" {
			log.Fatal("-domain needs -oidc-org or -oidc-email-domains: who may log in")
		}
		web, err := newWebFromFiles(WebConfig{Domain: *domain, Issuer: *issuer, OrgID: *org, EmailDomains: splitList(*emailDoms)},
			*oidcFile, *stateDir+"/web_secret", store)
		if err != nil {
			log.Fatal(err)
		}
		web.vmLock = &b.mu
		web.health = b.problems
		go func() { log.Fatal(http.ListenAndServe(*httpAddr, web)) }()
	}

	srv := b.server()
	signer, err := loadHostKey(*stateDir + "/boxd_host_ed25519")
	if err != nil {
		log.Fatal(err)
	}
	srv.AddHostKey(signer)

	var lns []net.Listener
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		lns = append(lns, ln)
	}
	if *tsName != "" {
		ts := &tsnet.Server{
			Hostname: *tsName,
			Dir:      *stateDir + "/tsnet",
			AuthKey:  readAuthKey(),
			Logf:     func(string, ...any) {},
		}
		ln, err := ts.Listen("tcp", ":22")
		if err != nil {
			log.Fatal(err)
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		log.Fatal("nothing to listen on")
	}
	for _, ln := range lns[1:] {
		go func() { log.Fatal(srv.Serve(ln)) }()
	}
	log.Printf("boxd %s: %d VMs, listening on %d addresses", version, len(vms), len(lns))
	log.Fatal(srv.Serve(lns[0]))
}

// readAuthKey is only needed for the first login of the tsnet node.
func readAuthKey() string {
	b, err := os.ReadFile("/etc/exe/ts-authkey")
	if err != nil {
		return os.Getenv("TS_AUTHKEY")
	}
	return strings.TrimSpace(string(b))
}

func loadHostKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, sk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := gossh.MarshalPrivateKey(sk, "box")
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(block)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return gossh.ParsePrivateKey(b)
}

// newWebFromFiles reads the OIDC client and the cookie secret, which is
// created on first start.
func newWebFromFiles(cfg WebConfig, oidcFile, secretFile string, store *Store) (*Web, error) {
	b, err := os.ReadFile(oidcFile)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "OIDC_CLIENT_ID":
			cfg.ClientID = v
		case "OIDC_CLIENT_SECRET":
			cfg.ClientSecret = v
		}
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New(oidcFile + ": missing OIDC_CLIENT_ID or OIDC_CLIENT_SECRET")
	}
	cfg.Secret, err = os.ReadFile(secretFile)
	if errors.Is(err, os.ErrNotExist) {
		cfg.Secret = make([]byte, 32)
		rand.Read(cfg.Secret)
		err = os.WriteFile(secretFile, cfg.Secret, 0o600)
	}
	if err != nil {
		return nil, err
	}
	return NewWeb(cfg, store), nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
