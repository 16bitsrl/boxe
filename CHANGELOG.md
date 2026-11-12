# Changelog

What changed in each release of boxe. Versions follow
[semantic versioning](https://semver.org); until 1.0, a minor version may
change how boxe is installed or run, and its notes say how to upgrade.

## v0.4.0 (2026-11-12)

- **A new desktop in the browser.** `https://<vm>--desktop.<domain>` is now
  streamed by [voxl](https://github.com/tembo/voxl) instead of KasmVNC: it
  needs far less bandwidth when pages scroll, follows the size of your
  window, plays the desktop's sound, shares the clipboard both ways (images
  too) and moves files to and from `~/Downloads` (the button at the top
  right). On a phone: tap to click, long press for the right button, two
  fingers to scroll.
- **Agents browse in the desktop's Chrome.** Claude Code, Codex, OpenCode and
  pi in the VM get the MCP tools of [browse](https://github.com/anomalyco/browse)
  (`browse`): they drive the Chrome you see on the desktop, so you watch the
  tabs they open and can take over, for a login for example. The first use
  starts the desktop. browse and its extension come with the image, so it
  works in VMs with limited egress too.
  - Both come with the official image published with this release, which
    every installation downloads within a day and uses for new VMs. Older
    VMs keep KasmVNC and get no browser tools.

## v0.3.0 (2026-11-12)

- **Subscriptions.** Each member can give boxe their own Claude or ChatGPT
  subscription, once, and Claude Code and Codex in all their VMs use it
  (`ssh boxe llm set claude`, `ssh boxe llm set chatgpt`, which tell how).
  OpenCode uses the Claude one through Claude Code, with its Claude Agent
  provider (`opencode auth login claude-agent` once in the VM).
  Like the API keys, the login stays on the host; Shelley and other programs
  keep using the keys. `ssh boxe llm usage` counts the subscriptions' tokens
  apart, without a cost.
  - VMs need the new `boxeuntu` to set Claude Code and Codex up for it, and
    OpenCode 2.2.3 or later for Claude Agent: the image `boxeuntu:20261110`
    and later have both, so VMs made from them are ready. In older VMs
    Codex and OpenCode keep using the API keys, and so does Claude Code if
    there is an Anthropic key.
  - boxed keeps the ChatGPT login refreshed by itself: give it a login made
    for boxe alone, not your usual `~/.codex/auth.json`, which would stop
    working.
- `ssh -t boxe llm set ...` shows its instructions as written, without a
  line broken at the 80th column, and reads an `auth.json` pasted whole.

## v0.2.0 (2026-11-08)

- **Official images.** boxe now publishes boxeuntu, its base image, as
  `ghcr.io/16bitsrl/boxeuntu:<date>`. Every day boxed downloads a newer one
  and makes it the default for new VMs; the host page of the panel shows
  which image new VMs start from and how its updates go. A host whose
  default is an image of its own keeps it.
  - Admins: `ssh boxe images update` checks at once, `ssh boxe images default
    <version>` picks the default by hand (going back, if a new image gives
    trouble: the cleanup keeps the version before the default, and the one
    `-base` names).
  - Upgrading: `scripts/deploy.sh` installs `scripts/build-base.sh` as
    `/usr/local/sbin/boxe-build-base`, which boxed runs to make an image of a
    download. To keep building boxeuntu yourself, set `-image-updates=`.
    Pointing `-base` at another image undoes an update's choice.

## v0.1.2 (2026-11-07)

- boxed now also deletes the Docker images that old image versions were
  built from, once those versions are gone: they filled the host's system
  disk. Admins can run the cleanup at once with `ssh boxe images prune`,
  which says what it removed.

## v0.1.1 (2026-11-07)

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
