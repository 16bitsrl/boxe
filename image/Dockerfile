# boxuntu, the base image of the microVMs: exeuntu (exe.dev, Apache 2.0) plus a layer of ours.
# Build: docker build -t boxuntu:<date> image/ && sudo scripts/build-base.sh boxuntu:<date> boxuntu-<date>
# Pinned: a rebuild must not change what VMs get. To update: pull, read what changed, change the digest.
FROM ghcr.io/boldsoftware/exeuntu@sha256:74db1d70f105846ab63a17aeca503f82309f565ead347ffccd3201ffd047ca36

# PID 1: applies the configuration disk from exe-host, then starts init.
COPY box-init /usr/local/bin/box-init
# On exe.dev SSH comes from their infrastructure; here we use openssh.
# box-init writes the host keys before boot; this service only generates
# them if they are missing.
COPY box-sshkeys.service /etc/systemd/system/box-sshkeys.service
COPY sshd-box.conf /etc/ssh/sshd_config.d/box.conf
# Fast boot: sshd starts early and login does not wait for the end of boot
# (/run/nologin is never created).
COPY ssh-early.conf /etc/systemd/system/ssh.service.d/early.conf
RUN : > /etc/tmpfiles.d/systemd-nologin.conf
# In Ubuntu 24.04 ssh.service requires ssh.socket, which exeuntu masks:
# drop the dependency and run sshd as a plain service.
RUN systemctl unmask ssh.service && \
    rm -rf /etc/systemd/system/ssh.service.requires /etc/systemd/system/sockets.target.wants/ssh.socket && \
    systemctl enable ssh.service box-sshkeys.service && \
    systemctl disable shelley.socket
# exeuntu's welcome talks about exe.dev and its services: use ours.
COPY box-welcome.sh /etc/profile.d/box-welcome.sh
RUN sed -i '/^# Print exe.dev message/,$d' /home/exedev/.bashrc && \
    usermod -c "box user" exedev
# Instructions for the coding agents in the image (Claude, Codex, opencode,
# Shelley): exeuntu's describe exe.dev. boxd writes ours, with this
# installation's addresses, through the configuration disk.
RUN install -d -o 1000 -g 1000 /home/exedev/.config/box
RUN for f in .config/shelley/AGENTS.md .claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md; do \
        ln -sfn /home/exedev/.config/box/AGENTS.md /home/exedev/$f; \
    done
