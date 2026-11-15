#!/bin/sh
# Removes sendspin-shelby from the speaker: the boot hook in /mnt/nv and the
# native-mode flag on the stick. After a reboot the speaker runs stock again.
#   sh /media/sda1/native/uninstall.sh
HOOK=/mnt/nv/rc.local
if [ -e "$HOOK" ]; then
    if grep -q "sendspin-shelby boot hook" "$HOOK"; then
        rm -f "$HOOK" && echo "removed $HOOK"
    else
        echo "$HOOK is not ours, left alone"
    fi
else
    echo "no $HOOK installed"
fi
rm -f /media/sda1/native-mode && echo "removed /media/sda1/native-mode"
rm -f /mnt/nv/native-boot.log /mnt/nv/native-boot.log.old
sync
echo "Reboot to finish:  reboot"
