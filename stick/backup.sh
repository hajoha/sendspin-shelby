#!/bin/sh
# Copies every flash partition (/dev/mtd0..N) to the stick. Read-only for the
# flash. Run it once before installing and keep the result somewhere safe.
#   sh /media/sda1/native/backup.sh
D=/media/sda1/flash-backup-$(cat /proc/variant 2>/dev/null)-$(date +%Y%m%d%H%M%S)
mkdir -p "$D" || exit 1
cp /proc/mtd "$D/mtd.txt"
cat /proc/cmdline > "$D/cmdline.txt"
fw_printenv > "$D/uboot-env.txt" 2>&1
for dev in $(sed -n 's/^\(mtd[0-9]*\):.*/\1/p' /proc/mtd); do
    printf '%s: ' "$dev"
    dd if=/dev/$dev of="$D/$dev.bin" bs=128k 2>&1 | tail -1
done
(cd "$D" && md5sum *.bin > md5sums.txt)
sync
echo "backup in $D"
echo "Copy it off the stick, e.g. from your computer:"
echo "  ssh ... 'tar cf - -C /media/sda1 $(basename "$D")' | tar xf -"
