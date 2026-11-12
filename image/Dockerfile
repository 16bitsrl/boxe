# boxeuntu, the base image of the microVMs: exeuntu (exe.dev, Apache 2.0) plus a layer of ours.
# Build: GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o image/boxeuntu ./cmd/boxeuntu && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o image/boxe-computer ./cmd/boxe-computer && docker build --build-arg BOXE_SOURCE=$(git describe --tags --always) -t boxeuntu:<date> image/ && sudo scripts/build-base.sh boxeuntu:<date> boxeuntu-<date>
# Pinned: a rebuild must not change what VMs get. To update: pull, read what changed, change the digest.
ARG BASE=ghcr.io/boldsoftware/exeuntu@sha256:74db1d70f105846ab63a17aeca503f82309f565ead347ffccd3201ffd047ca36

# voxl (github.com/tembo/voxl, MIT; the binary links x264, GPL) streams the
# desktop to the browser as H.264 over one WebSocket, with sound, clipboard
# and files. It has no release binaries: this stage builds a pinned commit
# (v0.3.4 and four more, the version we tried) on the base the image runs
# on, and only the binary leaves it. Rust and esbuild are pinned too.
FROM ${BASE} AS voxl
RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential git ca-certificates pkg-config clang libyuv-dev libx264-dev libopus-dev libpulse-dev \
        libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxcb-randr0-dev libxcb-damage0-dev libxcb-xtest0-dev \
        libxcb-xkb-dev libxkbcommon-dev libxcb-xinput-dev
ADD --checksum=sha256:b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55 \
    https://static.rust-lang.org/rustup/archive/1.29.3/x86_64-unknown-linux-gnu/rustup-init /tmp/rustup-init
ADD --checksum=sha256:9573bb2233aab0f9ea7647d5cca9726113cc1768de61d66b17267f4db84488f6 \
    https://registry.npmjs.org/@esbuild/linux-x64/-/linux-x64-0.28.2.tgz /tmp/esbuild.tgz
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo
RUN chmod 755 /tmp/rustup-init && /tmp/rustup-init -y -q --profile minimal --default-toolchain 1.99.1 --no-modify-path && \
    tar -xzf /tmp/esbuild.tgz -C /tmp && install -m 755 /tmp/package/bin/esbuild /usr/local/bin/esbuild
RUN git clone -q https://github.com/tembo/voxl /src && cd /src && \
    git checkout -q b00f3b8b8c156b14e0c75abdfcf5cae9a14a865f && \
    /opt/cargo/bin/cargo build -q --release --locked && \
    strip -o /voxl target/release/voxl

FROM ${BASE}

# exeuntu's user is exedev: ours is boxedev, same UID and permissions.
# exeuntu works in /home/exedev, and docker would make it again at every step.
WORKDIR /
COPY boxe-rename-user.sh /tmp/rename-user.sh
RUN bash /tmp/rename-user.sh && rm /tmp/rename-user.sh
WORKDIR /home/boxedev

# PID 1: applies the configuration disk from the host, then starts init.
COPY boxe-config.sh /usr/local/lib/boxe/config.sh
COPY boxe-init /usr/local/bin/boxe-init
# Answers the host over vsock: after a fork, takes the copy's own identity.
COPY boxe-agent /usr/local/sbin/boxe-agent
COPY boxe-agent.service /etc/systemd/system/boxe-agent.service
# On exe.dev SSH comes from their infrastructure; here we use openssh.
# boxe-init writes the host keys before boot; this service only generates
# them if they are missing.
COPY boxe-sshkeys.service /etc/systemd/system/boxe-sshkeys.service
COPY sshd-boxe.conf /etc/ssh/sshd_config.d/boxe.conf
# Fast boot: sshd starts early and login does not wait for the end of boot
# (/run/nologin is never created).
COPY ssh-early.conf /etc/systemd/system/ssh.service.d/early.conf
RUN : > /etc/tmpfiles.d/systemd-nologin.conf
# In Ubuntu 24.04 ssh.service requires ssh.socket, which exeuntu masks:
# drop the dependency and run sshd as a plain service.
RUN systemctl unmask ssh.service && \
    rm -rf /etc/systemd/system/ssh.service.requires /etc/systemd/system/sockets.target.wants/ssh.socket && \
    systemctl enable ssh.service boxe-sshkeys.service boxe-agent.service
# Shelley, the agent (Apache 2.0): exe.dev installs it in their VMs, so
# exeuntu has its units but not the program. Pinned like the base image.
ADD --checksum=sha256:0ae91dfc2ca214ff71f624c0b42a0eda2e6cf7361de2525be6f9a88aa8467769 \
    https://github.com/boldsoftware/shelley/releases/download/v0.1540.953300462/shelley_linux_amd64 /usr/local/bin/shelley
COPY shelley-socket.conf /etc/systemd/system/shelley.socket.d/boxe.conf
COPY shelley-service.conf /etc/systemd/system/shelley.service.d/boxe.conf
RUN chmod 755 /usr/local/bin/shelley && \
    install -d -o 1000 -g 1000 /home/boxedev/.config/shelley && \
    systemctl enable shelley.socket
# exe-scroll (MIT, by exe.dev): terminal sessions that survive a dropped
# connection, with scrollback; `scroll [name]` attaches or starts one.
ADD --checksum=sha256:56e80a95f63765aa4e3cadcac704f20cabfe848696e0d70f55e1f73664e93f4e \
    https://github.com/boldsoftware/exe.dev/releases/download/exe-scroll/v0.16.92023324/exe-scroll-linux-amd64 /usr/local/bin/exe-scroll
COPY scroll /usr/local/bin/scroll
COPY exe-scroll-LICENSE /usr/share/doc/exe-scroll/LICENSE
RUN chmod 755 /usr/local/bin/exe-scroll /usr/local/bin/scroll
# A desktop with a browser, for agents that try UIs while their user watches,
# at https://<vm>--desktop.<domain>: Xvfb, openbox, xterm, PulseAudio for its
# sound, and voxl, which streams it to the browser. It starts on first use.
# Agents drive it with boxe-computer (screenshots, and mouse and keyboard
# through xdotool), and its Google Chrome with browse (below).
# Chrome comes from Google's repository under Google's terms, at its latest
# version when the image is built.
COPY --from=voxl /voxl /usr/local/bin/voxl
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
        > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        google-chrome-stable xvfb pulseaudio libx264-164 libyuv0 libopus0 libpulse0 \
        openbox xterm fonts-noto-color-emoji xdotool && \
    rm -rf /var/lib/apt/lists/* && \
    LD_BIND_NOW=1 voxl --version
COPY boxe-desktop.sh /usr/local/lib/boxe/desktop.sh
COPY desktop.pa /usr/local/lib/boxe/desktop.pa
COPY desktop-client.conf /etc/pulse/client.conf.d/boxe-desktop.conf
COPY boxe-desktop.service boxe-desktop-web.service boxe-desktop-web.socket boxe-chrome.service /etc/systemd/system/
RUN chmod 755 /usr/local/lib/boxe/desktop.sh && systemctl enable boxe-desktop-web.socket
# browse (github.com/anomalyco/browse, MIT) drives that Chrome for agents,
# through its extension, so who watches the desktop sees what they do. Both
# come from browse's GitHub release, never from the Chrome Web Store: the
# extension is packed here with a key made for this build, and Chrome
# installs it by policy from the local file. The browse command
# (browse-wrapper.sh) accepts that extension and does not update itself:
# the CLI and the extension must be the same version, so both change here.
ADD --checksum=sha256:4a5a00eb15e9d8b126bed50f0499cab51e9c501bc9653866b3d002d35231051c \
    https://registry.npmjs.org/@opencode/browse-linux-x64/-/browse-linux-x64-0.0.16.tgz /tmp/browse.tgz
ADD --checksum=sha256:0d716106a36a42ab18d4abb2c3d449b88e20bc87e7321128da15feddde1a42c4 \
    https://github.com/anomalyco/browse/releases/download/v0.0.16/browse-extension-0.0.16.zip /tmp/browse-extension.zip
COPY browse-wrapper.sh /usr/local/bin/browse
COPY boxe-browse-pair.sh /usr/local/lib/boxe/browse-pair.sh
RUN set -e; ext=/usr/local/share/boxe/browse-extension; \
    tar -xzf /tmp/browse.tgz -C /tmp && install -m 755 /tmp/package/bin/browse /usr/local/lib/boxe/browse && \
    install -d "$ext" /tmp/browse-extension && \
    python3 -c 'import sys, zipfile; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])' /tmp/browse-extension.zip /tmp/browse-extension && \
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out /tmp/browse-extension.pem && \
    google-chrome --no-sandbox --pack-extension=/tmp/browse-extension --pack-extension-key=/tmp/browse-extension.pem && \
    install -m 644 /tmp/browse-extension.crx "$ext/browse.crx" && \
    id=$(openssl pkey -in /tmp/browse-extension.pem -pubout -outform DER | sha256sum | head -c 32 | tr 0-9a-f a-p) && \
    echo "$id" > "$ext/id" && \
    printf '<?xml version="1.0" encoding="UTF-8"?>\n<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0"><app appid="%s"><updatecheck codebase="file://%s/browse.crx" version="0.0.16"/></app></gupdate>\n' "$id" "$ext" > "$ext/update.xml" && \
    install -d /etc/opt/chrome/policies/managed && \
    printf '{"ExtensionInstallForcelist": ["%s;file://%s/update.xml"]}\n' "$id" "$ext" > /etc/opt/chrome/policies/managed/boxe.json && \
    chmod 755 /usr/local/bin/browse /usr/local/lib/boxe/browse-pair.sh && \
    rm -rf /tmp/browse.tgz /tmp/package /tmp/browse-extension* && \
    browse --version
# exeuntu masks udev (it is made for containers). A VM needs it: without it
# the ACPI power button is ignored and the host cannot shut the VM down cleanly.
RUN rm -f /etc/systemd/system/systemd-udevd.service /etc/systemd/system/systemd-udevd-control.service \
        /etc/systemd/system/systemd-udevd-kernel.service /etc/systemd/system/systemd-udev-trigger.service \
        /etc/systemd/system/systemd-udev-settle.service
# exeuntu's welcome talks about exe.dev and its services: use ours.
COPY boxe-welcome.sh /etc/profile.d/boxe-welcome.sh
RUN sed -i '/^# Print exe.dev message/,$d' /home/boxedev/.bashrc && \
    usermod -c "boxe user" boxedev
# The coding agents: boxeuntu (ours, forked from exeuntu's CLI) updates them
# and sets them up for the boxe gateway, at boot and when the owner's keys
# change, and gives them the computer tools of boxe-computer. Out: what
# exeuntu has for exe.dev's own services (its CLI, the OpenCode and pi
# plugins for its gateway, the unit for its setup scripts).
COPY boxeuntu /usr/local/bin/boxeuntu
COPY boxe-computer /usr/local/bin/boxe-computer
COPY boxe-agents.service /etc/systemd/system/boxe-agents.service
RUN chmod 755 /usr/local/bin/boxeuntu /usr/local/bin/boxe-computer && \
    systemctl disable exe-setup.service && \
    rm -rf /usr/local/bin/exeuntu /etc/opencode/plugins /home/boxedev/.pi/agent/extensions/exe-dev \
        /etc/systemd/system/exe-setup.service && \
    systemctl enable boxe-agents.service
# OpenCode runs Claude Code on a Claude subscription from 2.2.3 on; exeuntu
# has 2.2.0. Pinned like the base image.
RUN boxeuntu update opencode --version 2.2.7 && opencode --version | grep -qx 'opencode v2.2.7'
# Instructions for the coding agents in the image (Claude, Codex, opencode,
# pi, Shelley): exeuntu's describe exe.dev. boxed writes ours, with this
# installation's addresses, through the configuration disk.
RUN install -d -o 1000 -g 1000 /home/boxedev/.config/boxe
RUN for f in .config/shelley/AGENTS.md .claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md .pi/agent/AGENTS.md; do \
        ln -sfn /home/boxedev/.config/boxe/AGENTS.md /home/boxedev/$f; \
    done
# Nothing may still name exeuntu's user, nor be left of exe.dev's services.
RUN [ ! -e /home/exedev ] && [ ! -e /exe.dev ] && ! ls /etc/systemd/system/*/exe-* 2>/dev/null && ! grep -rlwI exedev /etc /home /usr/local/lib/boxe /var/lib/systemd /var/spool 2>/dev/null && \
    ! find / -xdev -lname '*/home/exedev*' 2>/dev/null | grep -q .
# The boxe it was built from (git describe), for ssh boxe images and for the VM.
ARG BOXE_SOURCE=
LABEL boxe.source=$BOXE_SOURCE
RUN printf 'BOXE_SOURCE=%s\n' "$BOXE_SOURCE" > /etc/boxe-image
