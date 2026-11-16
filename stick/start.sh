#!/bin/sh
# sendspin-shelby native-mode bring-up, started in the background by
# /mnt/nv/rc.local when the stick holds the "native-mode" flag file.
#
# 1. Wi-Fi with our own wpa_supplicant + udhcpc (the vendor NetManager is not
#    running). No IP within 90 s -> starts the vendor stack instead (fallback).
# 2. Codec + amp setup.
# 3. sendspin-go in a restart loop.
#
# Everything is logged to the stick, so it is readable on a laptop even when
# the speaker never got onto the network.
S=/media/sda1/native
LOG="$S/log.txt"
[ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ] && mv -f "$LOG" "$LOG.old"
exec >> "$LOG" 2>&1
t() { cut -d' ' -f1 /proc/uptime; }
echo "=== $(t) sendspin-shelby start.sh"

fallback() {
    echo "$(t) FALLBACK ($1): starting the vendor stack"
    [ -n "$WPA_PID" ] && kill $WPA_PID 2>/dev/null
    kill $(cat /run/udhcpc.wlan0.pid 2>/dev/null) 2>/dev/null
    sleep 2
    umount /etc/init.d/SoundTouch && /etc/init.d/SoundTouch start
    echo "$(t) fallback done rc=$?"
    sync
    exit 0
}

[ -f "$S/wpa.conf" ] || fallback "no $S/wpa.conf"

# --- Wi-Fi ----------------------------------------------------------------
# DHCP hook (the vendor udhcpc script talks to NetManager, which is not running).
cat > /run/udhcpc.native <<'EOS'
#!/bin/sh
case "$1" in
  deconfig) ifconfig "$interface" 0.0.0.0 ;;
  bound|renew)
    ifconfig "$interface" "$ip" netmask "${subnet:-255.255.255.0}"
    route del default 2>/dev/null
    for r in $router; do route add default gw "$r" dev "$interface"; break; done
    if [ -n "$dns" ]; then
      for d in $dns; do echo "nameserver $d"; done > /run/resolv.conf.native
      cat /run/resolv.conf.native > /etc/resolv.conf 2>/dev/null
    fi
    echo "dhcp $1: $ip router=$router" ;;
esac
EOS
chmod 755 /run/udhcpc.native

# shelby_local brings wlan0 up right after rc.local returns.
i=0; while [ ! -e /sys/class/net/wlan0 ] && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done
sleep 5
ifconfig wlan0 up
# Wi-Fi power saving causes audible dropouts in synchronised playback.
iw dev wlan0 set power_save off
# Optional file native/country: Wi-Fi regulatory domain, e.g. DE, GB, US.
COUNTRY=$(cat "$S/country" 2>/dev/null)
[ -n "$COUNTRY" ] && iw reg set "$COUNTRY"
HOST=$(cat "$S/hostname" 2>/dev/null || echo sendspin-shelby)

# Debug output to the stick (keys are not printed without -K).
/usr/local/sbin/wpa_supplicant -d -i wlan0 -D nl80211 -c "$S/wpa.conf" > "$S/wpa.log" 2>&1 &
WPA_PID=$!
echo "$(t) wpa_supplicant pid=$WPA_PID"
udhcpc -i wlan0 -b -p /run/udhcpc.wlan0.pid -s /run/udhcpc.native -x "hostname:$HOST"

i=0
while [ $i -lt 90 ] && ! ifconfig wlan0 | grep -q "inet addr"; do sleep 1; i=$((i+1)); done
/usr/local/sbin/wpa_cli -p /var/run/wpa_supplicant -i wlan0 status
ifconfig wlan0 | grep -q "inet addr" || {
    /usr/local/sbin/wpa_cli -p /var/run/wpa_supplicant -i wlan0 scan_results
    fallback "no IP after 90 s, check native/wpa.conf and native/wpa.log"
}
echo "--- $(t) network up after ${i}s"
ifconfig wlan0 | grep "inet addr"

# --- audio ----------------------------------------------------------------
# Optional file native/codec_mode:
#   dsp (default)  the vendor's rhino_sm2_normal.cfg with its sound tuning.
#                  It loads with the volume coefficient at 0 (silent); the
#                  player writes the volume into the codec's miniDSP.
#   passthrough    rhino_sm2_passthrough.cfg, no tuning, software volume.
# Then "fb" (flip the miniDSP coefficient buffers), as the vendor stack does.
# Amp power (gpio75) is already on at boot; mute_out (gpio72) stays 0 and
# the player unmutes it while audio flows.
# NEVER open /dev/snd/snd_shelby write-only: that kills TX DMA until reboot.
A=/sys/class/sound/snd_aic3256
MODE=$(cat "$S/codec_mode" 2>/dev/null || echo dsp)
CFG=rhino_sm2_normal.cfg
[ "$MODE" = passthrough ] && CFG=rhino_sm2_passthrough.cfg
echo -n 0 > $A/configured
echo -n $CFG > $A/configuredFile
i=0; while [ "$(cat $A/configured)" != 1 ] && [ $i -lt 30 ]; do usleep 100000; i=$((i+1)); done
echo -n fb > /proc/snd_aic3256
echo 0 > /sys/class/gpio/gpio72/value
echo "--- $(t) mode $MODE, codec configured=$(cat $A/configured) file=$(cat $A/configuredFile)"

# --- player ---------------------------------------------------------------
if [ ! -f "$S/sendspin-go" ] || [ ! -f "$S/player.yaml" ]; then
    echo "--- $(t) no $S/sendspin-go or $S/player.yaml, player not started"
    sync; exit 0
fi
L="$S/sendspin.log"
[ -f "$L" ] && [ "$(wc -c < "$L")" -gt 1048576 ] && mv -f "$L" "$L.old"
(
    while true; do
        echo "=== $(t) starting sendspin-go" >> "$L"
        # Optional file native/max_db: level at volume 100 in dB.
        #   dsp: miniDSP volume, default -40, never above -20.
        #   passthrough: software cap, default -24.
        # native/volume: the last volume, written by the player itself.
        SENDSPIN_SHELBY_MODE=$MODE SENDSPIN_SHELBY_MAX_DB=$(cat "$S/max_db" 2>/dev/null) \
        SENDSPIN_SHELBY_VOLUME_FILE="$S/volume" \
            nice -n -10 "$S/sendspin-go" --config "$S/player.yaml" >> "$L" 2>&1
        echo "=== $(t) sendspin-go exited rc=$?, restart in 5 s" >> "$L"
        sleep 5
    done
) &
echo "--- $(t) player supervisor started"
# The stock boot splash (/etc/init.d/rhino-boot-splash) runs the knight-rider
# LED pattern until the vendor stack kills it. That stack is not running, so
# end it here; on SIGTERM it exits cleanly and switches all LEDs off.
kill $(pidof led-splash-write) 2>/dev/null && echo "--- $(t) LED splash stopped"
sync
