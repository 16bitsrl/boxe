# Changelog

What changed in each release of boxe. Versions follow
[semantic versioning](https://semver.org); until 1.0, a minor version may
change how boxe is installed or run, and its notes say how to upgrade.

## Unreleased

- `ssh boxe images` shows the version of each image without its name again
  (`20261107`, not `boxeuntu-20261107`) and the boxe release it was built
  from. Build images with `--build-arg BOXE_SOURCE=$(git describe --tags
  --always)`: `build-base.sh` records it, and the VM finds it in
  `/etc/boxe-image`. Images built before show `-`. `--json` keeps the full
  version and adds `source`.

## v0.1.0 (2026-11-07)

The first versioned release: everything boxe does today.

### New in this release

- **Computer use.** Agents can see and drive a VM's desktop: screenshots,
  clicks, keys, typing (accents and other scripts included), scrolling and
  dragging, with the actions of Anthropic's computer use tool.
  - In the VM, Claude Code, Codex, OpenCode and pi get an MCP tool,
    `computer`, set up for them by `boxeuntu`; Shelley and scripts use the
    `boxe-computer` command.
  - From outside, `ssh boxe computer <vm> <action>` and `computer` over the
    HTTPS API (screenshots come back as `image/png`), for the same people as
    the desktop. A token allowed `computer` is as strong as one allowed `exec`.
  - Coordinates are those of the last screenshot, scaled to fit 1280x800. If
    someone resizes the desktop in the browser, the next action asks for a
    new screenshot instead of clicking in the wrong place.
  - Actions take turns, so two agents never mix their clicks or keys.

### Already there

- VMs: Cloud Hypervisor microVMs on ZFS clones, ready in about a second with
  `ssh boxe new`; `cp`, `fork` (memory included), `restart`, `rm`, time to
  live, images (`boxeuntu`, `laravel`) and their cleanup.
- Access: SSH as the CLI, with keys from GitHub; `ssh <vm>.boxe` through
  `boxed`; HTTPS on `<vm>.<domain>` and `<vm>--<port>.<domain>` behind the
  team's OIDC login; share links; `ssh boxe access` for other members.
- In the browser: the panel (VMs, live charts, console, host page for
  admins), the desktop with Chrome (KasmVNC), the terminal (`<vm>--term`) and
  the Shelley agent (`<vm>--agent`).
- Sleep: idle VMs save their memory and free RAM, wake up on the next
  connection, and wake up for their cron jobs and timers; a RAM limit per
  member and for the host.
- Agents: Shelley, Claude Code, Codex, OpenCode and pi in every VM, set up for
  the LLM gateway with each account's own API keys; `--agent <prompt>` hands
  a new VM a task.
- Integrations: secrets added on the way out by the host, GitHub over git and
  API, restricted egress.
- HTTPS API with tokens, `exec` in a VM.
- `boxed -front` keeps SSH sessions, forwards and WebSockets open while
  `boxed` restarts; `scripts/deploy.sh` with health check and rollback.
