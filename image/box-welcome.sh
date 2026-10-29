# Welcome message for interactive logins on a box VM.
if [[ $- == *i* ]]; then
    _name=$(hostname -s)
    _domain=$(cat /etc/box/web-domain 2>/dev/null)
    echo ""
    echo "You are on $_name. The disk is persistent. You have 'sudo'."
    echo ""
    if [[ -n $_domain ]]; then
        echo "Port 8000 is at https://$_name.$_domain (team login),"
        echo "ports 3000-9999 at https://$_name--<port>.$_domain."
    fi
    echo 'Docker works: docker run --rm alpine echo hello'
    echo 'Manage your VMs from your computer: ssh box help'
    echo ""
    unset _name _domain
fi
