<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="site/assets/logo-reverse.svg">
    <img src="site/assets/logo.svg" alt="boxe" height="56">
  </picture>
</h1>

**Start with one box. Raise it.**

Tiny Linux microVMs that start in about a second, on a host you own. Create
one over SSH; it gets its own HTTPS address behind your team's login. boxe is
a self-hosted take on [exe.dev](https://exe.dev): one Go daemon (`boxed`) on
one Linux host with KVM and ZFS.

[boxe.dev](https://boxe.dev) · [Install](#install) · [Security model](#security-model)

```
$ ssh boxe new
quick-fox ready in 0.8 s

  ssh quick-fox.boxe

  IP 10.42.0.17, 2 CPU, 2G RAM

$ ssh quick-fox.boxe          # a full Ubuntu VM: sudo, Docker, persistent disk
$ open https://quick-fox.boxe.example.com          # port 8000, behind team login
$ open https://quick-fox--3000.boxe.example.com    # any port 3000-9999
```

"Ready" means the VM's SSH server answers. It is measured by `boxed` from the
moment the command arrives, on our test host: an 8-vCPU VM with nested
virtualization on a Ryzen 9 8945HS. Typical runs take 0.7 to 1.0 s; a cold
cache or a busy host takes up to 2.5 s.

boxe is a small internal tool made public. It runs one host, with no high
availability, and it is not a multi-tenant cloud. Read [Security model](#security-model)
before you trust it with anything.

## How it works

```
             ssh boxe <command>, ssh <vm>.boxe          https://<vm>.boxe.example.com
                       │ (Tailscale)                        │
                       ▼                                    ▼
 host     ┌──────────────────── boxed ────────────────── TLS proxy (Caddy, ...)
          │ SSH server: commands, jump host to VMs   │  web: OIDC login, proxy to VM ports
          │ VM lifecycle, SQLite                     │
          └──────────────────────────────────────────┘
             │ ZFS clone (instant)      │ cloud-hypervisor per VM (unprivileged, sandboxed)
             ▼                          ▼
          zvol per VM  ◄────────── microVM ── tap ── br0 10.42.0.0/16 ── NAT ── internet
          config disk (read-only tar: hostname, SSH keys) ─► applied by boxe-init in the VM
```

- **VMs** are [Cloud Hypervisor](https://www.cloudhypervisor.org) microVMs
  booting a shared kernel binary directly (no firmware, no bootloader). Each
  VM boots its own copy of it.
- **Disks** are ZFS clones of a base image snapshot, so creating one is instant.
  The base image, *boxeuntu*, is exe.dev's [exeuntu](https://github.com/boldsoftware/exeuntu)
  plus a thin layer (`image/`).
- **Configuration** (hostname, machine-id, SSH host key and certificate,
  authorized keys) travels as a small read-only second disk. `boxe-init`, the
  VM's PID 1, applies it before systemd starts. The host never mounts a VM's
  disk, so it never parses data a VM wrote.
- **SSH is the CLI.** Users are GitHub usernames in `/etc/boxe/users`, and
  their keys come from `github.com/<user>.keys`. `ssh <vm>.boxe` goes through
  `boxed` as a jump host, only to the user's own VMs. VM host keys are signed
  by `boxed`, so one `@cert-authority` line in `known_hosts` trusts every VM.
- **HTTPS**: a TLS proxy in front sends `<domain>` and `*.<domain>` to `boxed`.
  `boxed` logs users in with OIDC, gives every VM host its own session cookie,
  and proxies to the VM with the user's email in `X-Box-Email`.
- **Network**: VMs reach the internet only. They cannot reach each other, the
  host, private networks, the tailnet or cloud metadata. Each tap may only
  send with its own MAC and IP.
- **Sleep**: with `-sleep-after`, a VM nobody uses (no SSH or web connection,
  little CPU and network) goes to sleep: Cloud Hypervisor saves its memory to
  `-sleep-dir` and exits, freeing RAM and CPU. The next `ssh <vm>.boxe` or web
  request wakes it up where it was, in about half a second: the memory is
  mapped copy-on-write and read from disk as the VM touches it. Only awake
  VMs count against a member's RAM (`-max-user-mem`): waking one up puts
  their other idle VMs to sleep if needed, the longest unused first.
- **Fork**: `ssh boxe fork` pauses a VM for a moment, saves its disk and
  memory together, and restores the copy from them. `boxe-agent` in the copy
  (over vsock) then applies the copy's own name, keys, MAC and IP address.
- **Agent**: every VM has [Shelley](https://github.com/boldsoftware/shelley)
  at `https://<vm>--agent.<domain>`, for its owner only. There is no shared
  LLM account: each member adds their own models and API keys in Shelley,
  and they stay in the VM (and in its copies). `--agent <prompt>`, last on
  the line of `new`, `cp` or `fork`, starts a conversation in the new VM and
  prints its link; typically you set up one VM with your keys and copy it.
- **Desktop**: every VM has a desktop with Google Chrome at
  `https://<vm>--desktop.<domain>` (KasmVNC), for its owner only. It starts on
  first use. Agents in the VM use it with `DISPLAY=:1`, for example to try a
  UI in a real browser while you watch.
- **Share links**: `ssh boxe share <vm> [port]` prints a link that shows that
  port of the VM to anyone who has it, without logging in. `unshare` makes
  the VM's links stop working; deleting the VM does too.
- **Images**: besides the default image, admins can build others on the host
  (for example `image/laravel/`, with PHP, Composer and Node).
  `ssh boxe images` lists them and `ssh boxe new --image laravel` starts
  from the newest version.
- **Panel**: `https://<domain>`, behind team login, lists your VMs (all of
  them for an admin) with their links and usage, charts of their CPU, RAM
  and disk over 24 hours, 7 or 30 days, and shows your account: the SSH keys
  boxe lets in, how to set up SSH, and logging out everywhere. Members make
  new VMs there and copy theirs. The owner of a VM, and an admin, can read
  its console, restart it or delete it (typing its name); everything else is
  done over SSH. Every action shows the `ssh boxe` command that does the
  same, and the panel runs exactly that command. Admins also get a page about the host: what `/healthz`
  would complain about, CPU, RAM and disk, the team with their keys and VMs,
  and the last backup run.

## Commands

```
ssh boxe new [name]                        create a VM (--cpu 2 --mem 2G --ttl 2h --image <name>)
ssh boxe ls [-a]                           your VMs and what they use (-a: everyone's)
ssh boxe rm <name>                         delete a VM (an admin: anyone's)
ssh boxe cp <name> [new-name]              copy a VM, even a running one
ssh boxe fork <name> [new-name]            copy a running VM with its memory
ssh boxe ttl <name> <2h|3d|off>            delete a VM by itself after a while
ssh boxe restart <name>                    power a VM off and on again (an admin: anyone's)
ssh boxe sleep <name>                      put a VM to sleep now
ssh boxe wake <name>                       wake a VM up (connecting does too)
ssh boxe keep-awake <name> on|off          never put this VM to sleep
ssh boxe share <name> [port]               a link to the VM for anyone (port 8000)
ssh boxe unshare <name>                    make the VM's links stop working
ssh boxe exec <name> <command>             run a command in a VM and print what it printed
ssh boxe images                            images to start a VM from
ssh boxe token new [--label x] [--exp 30d] [--cmds ls,new]
                                           a token for the HTTPS API, shown once
ssh boxe token ls                          your tokens (never the tokens themselves)
ssh boxe token rm <id|label>               delete a token
ssh boxe whoami                            your GitHub username, as boxe knows you
ssh boxe ssh-config                        config for ~/.ssh/config
ssh boxe known-hosts                       line for ~/.ssh/known_hosts
```

Add `--json` to a VM command for output a program can read: `ls` and
`images` print a list, `new`, `cp`, `fork`, `ttl` and `keep-awake` the VM,
`share` the link, the others the VM's name and new status, and errors
`{"error": "..."}`.

`ls` shows what each VM uses: CPU over the last minute (100% is one core),
the RAM it takes on the host (the memory the VM has touched, which grows up
to its RAM) and the space its disk takes in the pool.

`ssh boxe "cp base task1 --agent fix the login bug, don't touch the API"`:
everything after `--agent` is the prompt, as typed. Quote the whole command
when the prompt has characters your shell would read, like an apostrophe.

`ssh boxe exec <name> <command>` is `ssh <name>.boxe <command>` for programs
and the API: the command runs as `exedev`, the VM wakes up first if it
sleeps, it may run for 5 minutes, and boxe exits with its exit status. The
rest of the line goes to the VM's shell as typed, `--json` included.

`ssh boxe` alone, from a terminal, opens a prompt: type the commands
without `ssh boxe`, with history and tab completion of commands and of your
VM names. `exit` or ctrl-d leaves it. From a script, `ssh boxe` prints the
help instead.

Once per computer: `ssh boxe ssh-config >> ~/.ssh/config` and
`ssh boxe known-hosts >> ~/.ssh/known_hosts`.

## HTTPS API

Every command also works over HTTPS: POST the command line, as typed after
`ssh boxe`, to `https://<domain>/exec` with a token.

```
$ ssh boxe token new --label ci --exp 90d --cmds ls,new,exec
$ curl -X POST https://boxe.example.com/exec -H "Authorization: Bearer boxe_..." -d 'new --json'
$ curl -X POST https://boxe.example.com/exec -H "Authorization: Bearer boxe_..." -d 'exec quick-fox uptime'
```

- The command runs as the token's member, as over SSH, and the answer is
  what it printed (`text/plain`). Add `--json` for JSON.
- Status: 200 when it worked; 400 when boxe refused it (a mistake, a VM
  that is not yours) and 500 when it failed on the host, with the error as
  the body; 401 without a valid token, 403 when the token may not run that
  command or its member left the team. `exec` answers 422 when the command
  in the VM failed, and `X-Exit-Status` has its exit status.
- Tokens last 30 days unless `--exp` says otherwise (at most 365 days).
  `--cmds` limits what they may run. They never make, list or delete
  tokens: that is done over SSH only. boxe keeps only a hash of each token,
  so a lost token cannot be shown again: make another.
- Removing someone from `/etc/boxe/users` stops their tokens at once.
- The API takes no cookies, only the token: pages of VMs cannot use it
  with a member's browser.

## Security model

- **Members trust each other on the web.** Any logged-in member can reach
  any VM over HTTPS; SSH is owner-only. Apps in VMs get `X-Box-Email` from
  `boxed`, which strips any `X-Box-*` header a client sends. VMs are not
  reachable any other way, so the header can be trusted inside the VM. Apps
  must still protect themselves against CSRF from other VMs' pages.
- **Share links show one port to anyone who has them.** A visitor's
  requests carry no `X-Box-Email`, so an app that trusts the header treats
  them as anonymous. Links are random, work only on their own host and
  port, and stop working with `unshare` or when the VM is deleted; a
  visitor who opened one keeps a cookie for 7 days, checked against the
  link at every request.
- **The agent and the desktop are owner-only.** They are a shell in the VM,
  like SSH. boxed serves them only to the member whose email is next to the
  VM owner's GitHub username in `/etc/boxe/users`, and refuses requests that
  change something, and WebSockets, unless they come from their own page,
  so other VMs' pages cannot drive them with the owner's session. They
  cannot be shared.
- **The panel's actions come only from the panel.** VM pages are on the
  same site as `https://<domain>`, so the browser sends them the panel's
  cookie too. Creating, copying, restarting or deleting a VM is a POST that
  boxed accepts only from the panel's own origin and with a token bound to
  the session, which VM pages cannot read; the owner, or an admin, must type
  the VM's name to delete it.
- **VMs are untrusted.**
  - Each runs in its own Cloud Hypervisor process. The process runs as an
    unprivileged user with no capabilities, can open only its own disks,
    and has memory and CPU limits. It sees only its own files: not the other
    VMs' config disks (with their SSH keys), control sockets or saved memory,
    nor boxed's database and settings.
  - Session cookies are host-only (`__Host-`), so a VM can neither read nor
    set another host's session.
  - A copied disk is never mounted by the host.
- **Revocation.**
  - Removing a user from `/etc/boxe/users`, or a key from GitHub, closes
    their SSH connections at the next refresh (every 10 minutes).
  - Keys cached when GitHub is down expire after 7 days.
  - Web sessions end 12 hours after the login at the identity provider.
    `/logout` ends them on every VM at once, and `boxed -revoke <email>`
    does it for someone else.
- **Copies and backups are crash-consistent**, like a disk after a power cut.
- **Not covered:** the identity provider and the TLS proxy are yours to
  secure. A hypervisor escape would still reach the host as the unprivileged
  VM user.

## Install

Tested on Ubuntu 26.04 inside a Proxmox VM with nested virtualization.
Bare metal is simpler and faster (for example a Hetzner dedicated server).
Cloud VMs work only where nested virtualization is offered (GCP, Azure, AWS
C8i/M8i/R8i).

1. **Host:** `/dev/kvm`, a ZFS pool (`zpool create boxe /dev/...`, then
   `zfs create boxe/vms`, `zfs create boxe/images` and, for sleep and fork,
   `zfs create boxe/sleep`), Docker (to build the image), `nftables`,
   `iptables` and `restic` (for backups). On Ubuntu:
   `apt install zfsutils-linux docker.io nftables iptables restic`.
2. **Cloud Hypervisor** (v54 or later): from its
   [release](https://github.com/cloud-hypervisor/cloud-hypervisor/releases),
   `cloud-hypervisor-static` as `/usr/local/bin/cloud-hypervisor` and
   `ch-remote-static` as `/usr/local/bin/ch-remote` (sleep and fork use it).
3. **Guest kernel:** the `ch-7.2.8` branch of
   `github.com/cloud-hypervisor/linux`, with `kernel/config-7.2.8-boxe`.
   It builds in a few minutes:
   ```
   apt install build-essential flex bison bc libelf-dev libssl-dev
   git clone --depth 1 -b ch-7.2.8 https://github.com/cloud-hypervisor/linux
   cp kernel/config-7.2.8-boxe linux/.config
   make -C linux olddefconfig && make -C linux -j$(nproc) vmlinux
   install -D -m644 linux/vmlinux /var/lib/boxe/kernels/vmlinux-7.2.8-boxe
   ```
   That path is `boxed`'s default; a kernel elsewhere needs `-kernel`.
4. **Base image:**
   `docker build -t boxeuntu:<date> image/ && sudo scripts/build-base.sh boxeuntu:<date> boxeuntu-<date>`.
5. **Host setup:**
   - `useradd --system --no-create-home --shell /usr/sbin/nologin boxe-vm`.
   - Install `scripts/net-setup.sh` in `/usr/local/sbin`, with
     `/etc/boxe/net.conf` setting `BOXE_PROXY=<proxy IP>` if the TLS proxy is
     another machine.
   - Generate a key pair for the host to reach VMs:
     `ssh-keygen -t ed25519 -N '' -f /etc/boxe/id_ed25519`.
   - List the team in `/etc/boxe/users` (see `scripts/users.example`): a
     GitHub username per line, then the member's email at the identity
     provider (without it they get no panel, agent or desktop), then
     `admin` for an admin.
   - Install `boxe-net.service` and `boxed.service` from `scripts/` in
     `/etc/systemd/system`, then `systemctl enable --now boxe-net` and
     `systemctl enable boxed` (it starts at the first deploy).
6. **Settings:** `/etc/boxe/boxed.env` (see `scripts/boxed.env.example` and
   `boxed -help`).
   - **Image:** `-base boxe/images/boxeuntu-<date>@base`, the image of step 4.
   - **Sleep and fork** are off until you set `-sleep-dir /boxe/sleep`;
     add `-sleep-after 30m` to put unused VMs to sleep.
   - **SSH:** `boxed` joins your [Tailscale](https://tailscale.com) tailnet
     by itself as the node `boxe` (`-ts-hostname`), so `ssh boxe` and
     `ssh <vm>.boxe` work from every device on it. Put an auth key in
     `/etc/boxe/ts-authkey` before the first start, or open the login link
     that `journalctl -u boxed` shows. Tailscale on the host is not needed.
     Without Tailscale, set `-ts-hostname= -listen :2222` and give members
     a `Host boxe` entry with the host's address and `Port 2222` in
     `~/.ssh/config`.
   - **Web:** `-domain <domain>` and `-oidc-issuer <issuer URL>`, and an
     OIDC client in `/etc/boxe/oidc` (`OIDC_CLIENT_ID=`,
     `OIDC_CLIENT_SECRET=`), with redirect URI
     `https://<domain>/oauth/callback`. Any OIDC provider works with
     `-oidc-email-domains` (verified emails in those domains get in); with
     Zitadel, `-oidc-org` lets in the users of one organization instead.
   - DNS records and a certificate for both `<domain>` and `*.<domain>`,
     on a TLS proxy that forwards them to `boxed`'s `-http` address (default
     `:8080`; `net-setup.sh` only lets the proxy reach it). With Caddy on
     the host, for example (the wildcard needs a DNS challenge):
     ```
     boxe.example.com, *.boxe.example.com {
         tls { dns <provider> ... }
         reverse_proxy 127.0.0.1:8080
     }
     ```
7. **Deploy:** `scripts/deploy.sh <ssh-target>` builds `boxed` (it needs Go
   where you run it), installs it as a new version over SSH with `sudo`, and
   rolls back if it does not come up. `boxed` stops at start, saying why, if
   the kernel, `cloud-hypervisor` or `ch-remote` is missing.
8. **Backups (optional):** copy `scripts/backup.conf.example` to
   `/etc/boxe/backup.conf`, install `boxe-backup.sh` and `boxe-restore.sh` in
   `/usr/local/sbin`, install `boxe-backup.service` and `boxe-backup.timer`,
   and enable the timer.

## Operations

- **Deploy and roll back:** `scripts/deploy.sh <host>` and
  `scripts/deploy.sh <host> --rollback`. The last five versions stay in
  `/usr/local/lib/boxed/`.
- **New base image:** build it under a new name, then point `-base` at it.
  Images are never rebuilt in place, because VMs are clones of them.
  Existing VMs keep their image. Delete an old image once
  `zfs list -o clones` shows no VM uses it.
- **New guest kernel:** install it under a new name and point `-kernel`
  at it. Keep the old file until every VM has restarted on the new one
  (`ssh boxe restart <name>`): a VM started from it needs it to stop or
  go to sleep.
- **Other images:** build them from the base image, as
  `image/laravel/Dockerfile` explains, under names like `<name>-<date>`
  (or `<name>-<date>-2` for a second one that day) next to the base
  image. `ssh boxe new --image <name>` picks the newest. Only admins build
  images: a Dockerfile runs as root on the host. boxed deletes an older
  version on its own once no VM uses it; the default image and the newest
  of each name stay. The Docker images used to build them are not needed
  afterwards.
- **Backups:**
  - Every night `boxe-backup.timer` saves the state (database, keys, host
    CA, `/etc/boxe`) and every VM disk as a raw image to any restic
    repository.
  - Images deduplicate from one night to the next, so a second backup of an
    unchanged VM adds a few hundred KB.
  - `boxe-restore.sh <vm>` puts a disk back. Keep the restic password
    somewhere other than the host.
  - On a new host: install it as above (without starting `boxed`), put
    back `/etc/boxe/backup.conf`, then
    `restic restore latest --host boxe --tag state --target /`, move the
    restored `boxe.db` (under `/tmp`) to `/var/lib/boxe/`, run
    `boxe-restore.sh <vm>` for every VM and start `boxed`. VMs keep their
    names, keys and addresses.
- **People:**
  - Add a GitHub username to `/etc/boxe/users`; it takes effect within 10
    minutes, or right away with `systemctl restart boxed`.
  - To remove someone, delete the line and run `boxed -revoke <email>`.
    Their VMs stay until an admin deletes them.
- **Health:** `https://<domain>/healthz` answers `ok`, or 503 with what needs
  attention: failed VMs, low disk, stale GitHub keys, an old backup. Point
  any uptime monitor at it.
- **Logs:** `journalctl -u boxed` and `journalctl -u boxe-vm-<name>` (the VM's
  console).
- **After a host reboot:** `boxed` starts every VM again. A VM that fails to
  start shows as `failed` in `ssh boxe ls`; `ssh boxe restart <name>` retries it.

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE). The base
image builds on exeuntu by Bold Software (exe.dev) and ships their Shelley,
both also under the Apache License 2.0. It installs KasmVNC, under the GNU
GPL 2.0: whoever distributes a built image must also offer its source.
