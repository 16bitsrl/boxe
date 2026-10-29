# boxe 🥊

Your team's own [exe.dev](https://exe.dev): disposable Linux microVMs that
start in about a second, managed over SSH, with HTTPS and team login in front
of them. One Go daemon (`boxd`) on one Linux host with KVM and ZFS.

```
$ ssh box new
quick-fox ready in 0.8 s

  ssh quick-fox.box

$ ssh quick-fox.box          # a full Ubuntu VM: sudo, Docker, persistent disk
$ open https://quick-fox.box.example.com          # port 8000, behind team login
$ open https://quick-fox--3000.box.example.com    # any port 3000-9999
```

"Ready" means the VM's SSH server answers. It is measured by `boxd` from the
moment the command arrives, on our test host: an 8-vCPU VM with nested
virtualization on a Ryzen 9 8945HS. Typical runs take 0.7 to 1.0 s; a cold
cache or a busy host takes up to 2.5 s.

boxe is a small internal tool made public. It runs one host, with no high
availability, and it is not a multi-tenant cloud. Read [Security model](#security-model)
before you trust it with anything.

## How it works

```
             ssh box <command>, ssh <vm>.box          https://<vm>.box.example.com
                       │ (Tailscale)                        │
                       ▼                                    ▼
 exe-host ┌──────────────────── boxd ─────────────────── TLS proxy (Caddy, ...)
          │ SSH server: commands, jump host to VMs   │  web: OIDC login, proxy to VM ports
          │ VM lifecycle, SQLite                     │
          └──────────────────────────────────────────┘
             │ ZFS clone (instant)      │ cloud-hypervisor per VM (unprivileged, sandboxed)
             ▼                          ▼
          zvol per VM  ◄────────── microVM ── tap ── br0 10.42.0.0/16 ── NAT ── internet
          config disk (read-only tar: hostname, SSH keys) ─► applied by box-init in the VM
```

- **VMs** are [Cloud Hypervisor](https://www.cloudhypervisor.org) microVMs
  booting a shared kernel binary directly (no firmware, no bootloader). Each
  VM boots its own copy of it.
- **Disks** are ZFS clones of a base image snapshot, so creating one is instant.
  The base image, *boxuntu*, is exe.dev's [exeuntu](https://github.com/boldsoftware/exeuntu)
  plus a thin layer (`image/`).
- **Configuration** (hostname, machine-id, SSH host key and certificate,
  authorized keys) travels as a small read-only second disk. `box-init`, the
  VM's PID 1, applies it before systemd starts. The host never mounts a VM's
  disk, so it never parses data a VM wrote.
- **SSH is the CLI.** Users are GitHub usernames in `/etc/exe/users`, and
  their keys come from `github.com/<user>.keys`. `ssh <vm>.box` goes through
  `boxd` as a jump host, only to the user's own VMs. VM host keys are signed
  by `boxd`, so one `@cert-authority` line in `known_hosts` trusts every VM.
- **HTTPS**: a TLS proxy in front sends `<domain>` and `*.<domain>` to `boxd`.
  `boxd` logs users in with OIDC, gives every VM host its own session cookie,
  and proxies to the VM with the user's email in `X-Box-Email`.
- **Network**: VMs reach the internet only. They cannot reach each other, the
  host, private networks, the tailnet or cloud metadata. Each tap may only
  send with its own MAC and IP.

## Commands

```
ssh box new [name] [--cpu N] [--mem 2G]   create a VM
ssh box ls [-a]                           your VMs (-a: everyone's)
ssh box rm <name>                         delete a VM
ssh box cp <name> [new-name]              copy a VM, even a running one
ssh box restart <name>                    power a VM off and on again
ssh box ssh-config                        config for ~/.ssh/config
ssh box known-hosts                       line for ~/.ssh/known_hosts
```

Once per computer: `ssh box ssh-config >> ~/.ssh/config` and
`ssh box known-hosts >> ~/.ssh/known_hosts`.

## Security model

- **Members trust each other on the web.** Any logged-in member can reach
  any VM over HTTPS; SSH is owner-only. Apps in VMs get `X-Box-Email` from
  `boxd`, which strips any `X-Box-*` header a client sends. VMs are not
  reachable any other way, so the header can be trusted inside the VM. Apps
  must still protect themselves against CSRF from other VMs' pages.
- **VMs are untrusted.**
  - Each runs in its own Cloud Hypervisor process. The process runs as an
    unprivileged user with no capabilities, can open only its own disks,
    and has memory and CPU limits.
  - Session cookies are host-only (`__Host-`), so a VM can neither read nor
    set another host's session.
  - A copied disk is never mounted by the host.
- **Revocation.**
  - Removing a user from `/etc/exe/users`, or a key from GitHub, closes
    their SSH connections at the next refresh (every 10 minutes).
  - Keys cached when GitHub is down expire after 7 days.
  - Web sessions end 12 hours after the login at the identity provider.
    `/logout` ends them on every VM at once, and `boxd -revoke <email>`
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

1. **Host:** `/dev/kvm`, a ZFS pool (`zpool create exe /dev/...`, then
   `zfs create exe/vms` and `zfs create exe/images`), Docker (to build the
   image), `nftables`, `iptables`, `restic` (for backups), and
   [Tailscale](https://tailscale.com) if `ssh box` should live on your
   tailnet.
2. **Cloud Hypervisor:** the static `cloud-hypervisor` binary in
   `/usr/local/bin` (v54 or later).
3. **Guest kernel:** build the `ch-*` branch of
   `github.com/cloud-hypervisor/linux` with `kernel/config-*`, and install
   `vmlinux` in `/var/lib/exe/kernels/`.
4. **Base image:**
   `docker build -t boxuntu:<date> image/ && sudo scripts/build-base.sh boxuntu:<date> boxuntu-<date>`.
5. **Host setup:**
   - `useradd --system --no-create-home --shell /usr/sbin/nologin exe-vm`.
   - Install `scripts/net-setup.sh` in `/usr/local/sbin`, with
     `/etc/exe/net.conf` setting `BOX_PROXY=<proxy IP>` if the TLS proxy is
     another machine.
   - Generate a key pair for the host to reach VMs (`/etc/exe/id_ed25519`).
   - List the team in `/etc/exe/users` (see `scripts/users.example`).
   - Install the units in `scripts/` and enable `exe-net` and `boxd`.
6. **Settings:** `/etc/exe/boxd.env` (see `scripts/boxd.env.example` and
   `boxd -help`).
   - For the web front end you need an OIDC client in `/etc/exe/oidc`
     (`OIDC_CLIENT_ID=`, `OIDC_CLIENT_SECRET=`), with redirect URI
     `https://<domain>/oauth/callback`.
   - You also need a wildcard DNS record and certificate for `*.<domain>`
     on a TLS proxy that forwards to `boxd`'s `-http` address (default
     `:8080`; `net-setup.sh` only lets the proxy reach it).
7. **Deploy:** `scripts/deploy.sh <ssh-target>` builds `boxd`, installs it as
   a new version, and rolls back if it does not come up.
8. **Backups (optional):** copy `scripts/backup.conf.example` to
   `/etc/exe/backup.conf`, install `box-backup.sh` and `box-restore.sh`, and
   enable `box-backup.timer`.

## Operations

- **Deploy and roll back:** `scripts/deploy.sh <host>` and
  `scripts/deploy.sh <host> --rollback`. The last five versions stay in
  `/usr/local/lib/boxd/`.
- **New base image:** build it under a new name, then point `-base` at it.
  Images are never rebuilt in place, because VMs are clones of them.
  Existing VMs keep their image. Delete an old image once
  `zfs list -o clones` shows no VM uses it.
- **Backups:**
  - Every night `box-backup.timer` saves the state (database, keys, host
    CA, `/etc/exe`) and every VM disk as a raw image to any restic
    repository.
  - Images deduplicate from one night to the next, so a second backup of an
    unchanged VM adds a few hundred KB.
  - `box-restore.sh <vm>` puts a disk back. Keep the restic password
    somewhere other than the host.
- **People:**
  - Add a GitHub username to `/etc/exe/users`; it takes effect within 10
    minutes, or right away with `systemctl restart boxd`.
  - To remove someone, delete the line and run `boxd -revoke <email>`.
    Their VMs stay until an admin deletes them.
- **Health:** `https://<domain>/healthz` answers `ok`, or 503 with what needs
  attention: failed VMs, low disk, stale GitHub keys, an old backup. Point
  any uptime monitor at it.
- **Logs:** `journalctl -u boxd` and `journalctl -u exe-vm-<name>` (the VM's
  console).
- **After a host reboot:** `boxd` starts every VM again. A VM that fails to
  start shows as `failed` in `ssh box ls`; `ssh box restart <name>` retries it.

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE). The base
image builds on exeuntu by Bold Software (exe.dev), also under the Apache
License 2.0.
