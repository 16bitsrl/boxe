# boxeuntu, the base image of the microVMs: exeuntu (exe.dev, Apache 2.0) plus a layer of ours.
# Build: docker build -t boxeuntu:<date> image/ && sudo scripts/build-base.sh boxeuntu:<date> boxeuntu-<date>
# Pinned: a rebuild must not change what VMs get. To update: pull, read what changed, change the digest.
FROM ghcr.io/boldsoftware/exeuntu@sha256:74db1d70f105846ab63a17aeca503f82309f565ead347ffccd3201ffd047ca36

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
    install -d -o 1000 -g 1000 /home/exedev/.config/shelley && \
    systemctl enable shelley.socket
# A desktop with a browser, for agents that try UIs while their user watches:
# KasmVNC (an X server that is also the desktop in the browser), openbox,
# xterm, Google Chrome (the browser agents' tools use by default), at
# https://<vm>--desktop.<domain>. It starts on first use.
# Chrome comes from Google's repository under Google's terms, at its latest
# version when the image is built. KasmVNC is pinned like the base image.
ADD --checksum=sha256:89a5d1fbe9e032b122202be158a2b25f15c97e0a5b8dcf92a5e6835c7b7b2acd \
    https://github.com/kasmtech/KasmVNC/releases/download/v1.5.2/kasmvncserver_ubuntu_noble_1.5.2_amd64.deb /tmp/kasmvnc.deb
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
        > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        google-chrome-stable /tmp/kasmvnc.deb openbox xterm fonts-noto-color-emoji && \
    rm -rf /var/lib/apt/lists/* /tmp/kasmvnc.deb
COPY boxe-desktop.sh /usr/local/lib/boxe/desktop.sh
COPY boxe-desktop.service boxe-desktop-web.service boxe-desktop-web.socket /etc/systemd/system/
RUN chmod 755 /usr/local/lib/boxe/desktop.sh && systemctl enable boxe-desktop-web.socket
# exeuntu masks udev (it is made for containers). A VM needs it: without it
# the ACPI power button is ignored and the host cannot shut the VM down cleanly.
RUN rm -f /etc/systemd/system/systemd-udevd.service /etc/systemd/system/systemd-udevd-control.service \
        /etc/systemd/system/systemd-udevd-kernel.service /etc/systemd/system/systemd-udev-trigger.service \
        /etc/systemd/system/systemd-udev-settle.service
# exeuntu's welcome talks about exe.dev and its services: use ours.
COPY boxe-welcome.sh /etc/profile.d/boxe-welcome.sh
RUN sed -i '/^# Print exe.dev message/,$d' /home/exedev/.bashrc && \
    usermod -c "boxe user" exedev
# Instructions for the coding agents in the image (Claude, Codex, opencode,
# Shelley): exeuntu's describe exe.dev. boxed writes ours, with this
# installation's addresses, through the configuration disk.
RUN install -d -o 1000 -g 1000 /home/exedev/.config/boxe
RUN for f in .config/shelley/AGENTS.md .claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md; do \
        ln -sfn /home/exedev/.config/boxe/AGENTS.md /home/exedev/$f; \
    done
