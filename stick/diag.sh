#!/bin/sh
# Read-only status report.  sh /media/sda1/native/diag.sh
S=/media/sda1/native
P=/proc/snd_shelby
A=/sys/class/sound/snd_aic3256
echo "=== uptime: $(uptime)"
echo "=== variant: $(cat /proc/variant 2>/dev/null)"
if pidof APServer > /dev/null; then
    echo "=== mode: STOCK (vendor stack running)"
else
    echo "=== mode: NATIVE"
fi
if [ ! -f /mnt/nv/rc.local ]; then hook="not installed"
elif grep -q 'sendspin-shelby boot hook' /mnt/nv/rc.local; then hook="installed"
else hook="FOREIGN /mnt/nv/rc.local (another mod?)"; fi
echo "boot hook: $hook"
echo "flag: $([ -e /media/sda1/native-mode ] && echo present || echo absent)"
echo "=== boot hook log"; tail -4 /mnt/nv/native-boot.log 2>/dev/null
echo "=== network"; ifconfig wlan0 2>/dev/null | grep -E "inet addr|RUNNING"
echo "=== codec"
for f in configured configuredFile runtime_pm; do echo "  $f=$(cat $A/$f 2>/dev/null)"; done
echo "  amp power gpio75=$(cat /sys/class/gpio/gpio75/value) mute_out gpio72=$(cat /sys/class/gpio/gpio72/value) (1 = unmuted)"
a=$(sed -n 's/.*txDoneCount: *//p' $P); sleep 1; b=$(sed -n 's/.*txDoneCount: *//p' $P)
echo "=== TX DMA: $((b - a)) buffers/s (about 93 = running at 48 kHz, 0 = stopped)"
grep txUnderflows $P
echo "=== player"
pidof sendspin-go > /dev/null && echo "  sendspin-go running (pid $(pidof sendspin-go))" || echo "  sendspin-go NOT running"
tail -8 "$S/sendspin.log" 2>/dev/null
echo "=== start.sh log"; tail -6 "$S/log.txt" 2>/dev/null
