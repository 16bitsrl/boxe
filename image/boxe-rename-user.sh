#!/bin/bash
# Renames exeuntu's user exedev to boxedev (same UID 1000, home
# /home/boxedev). Run as root by the image build and, once, in VMs made
# before the rename:
#   ssh <vm>.boxe 'sudo bash -s -- --keep-old-home' < image/boxe-rename-user.sh
# It edits the account files directly instead of using usermod, so a VM in
# use keeps running: processes go by UID, not by name. --keep-old-home leaves
# /home/exedev as a link to the new home, for paths saved in the user's own
# files (projects, containers, crontabs).
# Every step does only what is left to do: a run that stopped halfway is
# finished by running it again. It says done only once all of it is.
set -euo pipefail
old=exedev new=boxedev
keep=0
[ "${1:-}" = --keep-old-home ] && keep=1

# Every file of the system that names the user, as a word (exeuntu's
# X-Exedev-Userid header is not one).
named() {
	grep -rlwI "$old" /etc/passwd /etc/passwd- /etc/shadow /etc/shadow- /etc/group /etc/group- \
		/etc/gshadow /etc/gshadow- /etc/subuid /etc/subgid /etc/sudoers /etc/sudoers.d \
		/etc/crontab /etc/cron.d /etc/systemd/system /etc/tmpfiles.d /etc/profile.d /usr/local/lib/boxe 2>/dev/null || true
}
user_of() { awk -F: -v u="$1" '$1 == u' /etc/passwd; }

# Before changing anything: a state this script would not know how to end.
if [ -n "$(user_of $old)" ] && [ -n "$(user_of $new)" ]; then
	echo "both $old and $new exist: nothing changed" >&2
	exit 1
fi
if [ -z "$(user_of $old)" ] && [ -z "$(user_of $new)" ]; then
	echo "no user $old nor $new" >&2
	exit 1
fi
if [ -d "/home/$old" ] && [ ! -L "/home/$old" ] && [ -e "/home/$new" ]; then
	echo "both /home/$old and /home/$new exist: nothing changed" >&2
	exit 1
fi

# GNU sed -i keeps owner and mode.
for f in $(named); do
	sed -i "s/\b$old\b/$new/g" "$f"
done
if [ -d "/home/$old" ] && [ ! -L "/home/$old" ]; then
	mv -T "/home/$old" "/home/$new"
fi
f=/home/$new/.pki/nssdb/pkcs11.txt
if [ -f "$f" ] && grep -q "/home/$old/" "$f"; then
	sed -i "s#/home/$old/#/home/$new/#g" "$f"
fi
# Links into the old home (exeuntu has some, for pi), not those in containers.
find / -xdev \( -path /var/lib/docker -o -path /var/lib/containerd \) -prune -o -lname "/home/$old/*" -print |
	while read -r l; do
		t=$(readlink "$l")
		ln -sfn "/home/$new/${t#/home/$old/}" "$l"
	done
for d in /var/lib/systemd/linger /var/spool/cron/crontabs /var/mail; do
	if [ -e "$d/$old" ] && [ ! -e "$d/$new" ]; then
		mv -T "$d/$old" "$d/$new"
	fi
done
if [ $keep = 1 ] && [ ! -e "/home/$old" ]; then
	ln -s "/home/$new" "/home/$old"
fi
[ $keep = 1 ] && systemctl daemon-reload 2>/dev/null || true

# Done only if it all is.
home=$(user_of $new | cut -d: -f6)
if [ "$home" != "/home/$new" ] || [ ! -d "/home/$new" ] || [ -L "/home/$new" ] || [ -n "$(named)" ]; then
	echo "the rename is not complete: home ${home:-none}, still named in: $(named | tr '\n' ' ')" >&2
	exit 1
fi
echo "$old is now $new"
