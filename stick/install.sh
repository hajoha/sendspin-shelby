#!/bin/sh
# sendspin-shelby installer. Run ON THE SPEAKER, booted stock, with the stick in:
#   sh /media/sda1/native/install.sh
#
# Installs only /mnt/nv/rc.local (the writable flash partition) and creates the
# native-mode flag on the stick. Nothing on the read-only system or in U-Boot
# is changed. Undo with uninstall.sh.
set -u
S=/media/sda1/native
HOOK=/mnt/nv/rc.local
MARK="sendspin-shelby boot hook"
FORCE=0
[ "${1:-}" = "--force" ] && FORCE=1

die() { echo "ERROR: $*" >&2; exit 1; }
ok() { echo "  ok: $*"; }

echo "sendspin-shelby install"

[ -d "$S" ] || die "$S not found: is the stick mounted at /media/sda1 with the native/ folder on it?"

variant=$(cat /proc/variant 2>/dev/null)
if [ "$variant" != rhino ]; then
    [ $FORCE = 1 ] || die "this speaker reports variant '$variant'; only 'rhino' is verified. Re-run with --force to try anyway."
    echo "  WARNING: variant '$variant' is untested"
fi
ok "variant $variant"

for f in rhino_sm2_normal.cfg rhino_sm2_passthrough.cfg; do
    [ -f /lib/firmware/$f ] || die "/lib/firmware/$f missing: different firmware than expected"
done
[ -c /dev/snd/snd_shelby ] && [ -c /dev/snd_aic3256 ] || die "snd_shelby / snd_aic3256 devices missing"
[ -x /usr/local/sbin/wpa_supplicant ] || die "/usr/local/sbin/wpa_supplicant missing"
ok "firmware files and devices present"

for f in sendspin-go start.sh rc.local; do
    [ -f "$S/$f" ] || die "$S/$f missing: copy the whole release zip onto the stick"
done
"$S/sendspin-go" -h > /dev/null 2>&1 || die "$S/sendspin-go does not run on this speaker"
ok "player binary runs"

[ -f "$S/wpa.conf" ] || die "$S/wpa.conf missing: copy wpa.conf.example to wpa.conf and fill in your Wi-Fi"
grep -q 'ssid=' "$S/wpa.conf" && grep -q 'psk=' "$S/wpa.conf" || die "$S/wpa.conf needs ssid= and psk="
grep -v '^[[:space:]]*#' "$S/wpa.conf" | grep -q 'YOUR_' && die "$S/wpa.conf still contains the example placeholders"
ok "wpa.conf"

[ -f "$S/player.yaml" ] || die "$S/player.yaml missing: copy player.yaml.example to player.yaml"
grep -q '^audio_device: */dev/snd/snd_shelby' "$S/player.yaml" || die "player.yaml must contain 'audio_device: /dev/snd/snd_shelby'"
ok "player.yaml"

if [ -e "$HOOK" ] && ! grep -q "$MARK" "$HOOK"; then
    die "$HOOK exists and is not ours (another mod?). Remove it yourself first if you are sure."
fi

cp "$S/rc.local" "$HOOK.new" && sh -n "$HOOK.new" && chmod 755 "$HOOK.new" && mv -f "$HOOK.new" "$HOOK" \
    || { rm -f "$HOOK.new"; die "could not install $HOOK"; }
sync
ok "installed $HOOK"

touch /media/sda1/native-mode && sync
ok "created /media/sda1/native-mode"

cat <<EOF

Done. Reboot to start native mode:   reboot

Then, after about 2 minutes, the player shows up in Music Assistant.
Back to stock for one boot: unplug the stick (or delete /media/sda1/native-mode), reboot.
Remove completely: sh $S/uninstall.sh
EOF
