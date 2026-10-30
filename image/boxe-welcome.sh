# Welcome message for interactive logins on a boxe VM.
if [[ $- == *i* ]]; then
    _name=$(hostname -s)
    _domain=$(cat /etc/boxe/web-domain 2>/dev/null)
    echo ""
    echo "You are on $_name. The disk is persistent. You have 'sudo'."
    echo ""
    if [[ -n $_domain ]]; then
        echo "Port 8000 is at https://$_name.$_domain (team login),"
        echo "ports 3000-9999 at https://$_name--<port>.$_domain."
        echo "Shelley, the agent, is at https://$_name--agent.$_domain (yours only),"
        echo "a desktop with a browser at https://$_name--desktop.$_domain (DISPLAY=:1)."
    fi
    if [[ -e /etc/boxe/sleeps ]]; then
        echo "Unused for a while, the VM goes to sleep and wakes up when you connect."
        echo "Cron jobs and systemd timers still run: it wakes up for them. For a"
        echo "server that must answer at once: ssh boxe keep-awake $_name on"
    fi
    echo 'Docker works: docker run --rm alpine echo hello'
    echo 'Manage your VMs from your computer: ssh boxe help'
    echo ""
    unset _name _domain
fi
