# Applies the configuration the host attaches as a second, read-only disk
# (a tar archive: hostname, machine-id, SSH keys, network). Sourced by
# boxe-init at boot and by boxe-agent when the host asks.
#
# An archive is applied once: etc/boxe/id changes every time the host writes
# a new one (new VM, copy, fork), so later reboots keep the user's own changes.
boxe_cfg=/dev/vdb

# boxe_config_id prints the id of the archive on the configuration disk.
boxe_config_id() {
    local id
    id=$(tar -xOf "$boxe_cfg" etc/boxe/id 2>/dev/null) || { echo "cannot read the configuration" >&2; return 1; }
    [[ $id =~ ^[0-9a-f]{32}$ ]] || { echo "invalid configuration" >&2; return 1; }
    echo "$id"
}

# boxe_apply_config applies the archive if it is new. Returns 2 if there was
# nothing to apply, 1 on failure.
boxe_apply_config() {
    local id f
    [[ -b $boxe_cfg ]] || { echo "no configuration disk" >&2; return 1; }
    id=$(boxe_config_id) || return 1
    [[ $id != "$(cat /etc/boxe/id 2>/dev/null)" ]] || return 2
    for f in etc/hostname etc/machine-id etc/ssh/ssh_host_ed25519_key home/exedev/.ssh/authorized_keys; do
        tar -tf "$boxe_cfg" "$f" >/dev/null 2>&1 || { echo "configuration without $f" >&2; return 1; }
    done
    rm -f /etc/ssh/ssh_host_*
    # tar replaces files (resolv.conf may be a symlink: it is replaced, not
    # followed) and keeps directories that already exist, such as ~/.ssh.
    tar -xf "$boxe_cfg" -C / --same-owner --same-permissions --exclude etc/boxe/id ||
        { echo "could not apply the configuration" >&2; return 1; }
    # The marker last: a configuration applied halfway is applied again.
    tar -xf "$boxe_cfg" -C / --same-owner --same-permissions etc/boxe/id ||
        { echo "could not apply the configuration" >&2; return 1; }
    sync
}
