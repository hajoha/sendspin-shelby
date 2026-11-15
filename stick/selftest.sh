#!/bin/sh
# Native mode only: plays a quiet 440 Hz test tone for 6 s, bypassing the
# player, to check the audio path. Stops the player; reboot afterwards.
#   sh /media/sda1/native/selftest.sh
S=/media/sda1/native
P=/proc/snd_shelby
MUTE=/sys/class/gpio/gpio72/value
c() { sed -n 's/.*txDoneCount: *\([0-9]*\).*/\1/p' $P; }

pidof APServer > /dev/null && { echo "the vendor stack is running (stock mode): selftest only works in native mode"; exit 1; }
[ -f "$S/tone-20dB.raw" ] || { echo "$S/tone-20dB.raw missing"; exit 1; }

for p in $(ps w | grep "[s]h /media/sda1/native/start.sh" | awk '{print $1}'); do kill $p; done
killall sendspin-go 2>/dev/null; sleep 2
a=$(c); sleep 1; b=$(c)
[ -n "$a" ] && [ -n "$b" ] && [ "$b" -gt "$a" ] || { echo "TX DMA is not running -> reboot and try again"; exit 1; }

MODE=$(cat "$S/codec_mode" 2>/dev/null || echo dsp)
if [ "$MODE" != passthrough ]; then
    # Volume coefficient block for -57.5 dB, as the vendor stack writes it:
    # into both adaptive buffers (write, flip via SEEK_END = "fb", write).
    blk='\000\025\277\000\306\165\302\000\000\000\000\000\000\000\000\000\100\000\000\000\062\326\105\000\062\326\105\000\000\000\000\000'
    printf "$blk" | dd of=/dev/snd_aic3256 bs=16 seek=705 count=2 conv=notrunc 2>/dev/null
    echo -n fb > /proc/snd_aic3256
    printf "$blk" | dd of=/dev/snd_aic3256 bs=16 seek=705 count=2 conv=notrunc 2>/dev/null
fi

echo "playing 6 s test tone (mode $MODE)"
exec 4<> /dev/snd/snd_shelby || { echo "cannot open /dev/snd/snd_shelby"; exit 1; }
echo 1 > $MUTE
dd if="$S/tone-20dB.raw" bs=4096 >&4 2>/dev/null
echo 0 > $MUTE
exec 4>&-
echo "done. Reboot to restart the player:  reboot"
