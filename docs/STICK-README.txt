sendspin-shelby - native Sendspin player, USB stick contents
==============================================================

Full documentation: https://github.com/hajoha/sendspin-shelby

1. On your computer (stick formatted FAT32, this zip unpacked onto its root):
     native/wpa.conf.example   -> copy to native/wpa.conf,    fill in your Wi-Fi
     native/player.yaml.example -> copy to native/player.yaml, set the name
     optional: native/country   two-letter Wi-Fi country code, e.g. DE

2. Plug the stick into the speaker's micro-USB service port (USB OTG
   adapter), then unplug the speaker's power for 10 s and plug it back in.
   The file "remote_services" on this stick makes the stock firmware start
   ssh and telnet (root, no password, only while this stick is plugged in).

3. ssh into the speaker (find its IP in your router):
     ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedKeyTypes=+ssh-rsa root@SPEAKER_IP
   then:
     sh /media/sda1/native/backup.sh      (once: copy of the whole flash)
     sh /media/sda1/native/install.sh
     reboot

4. About 2 minutes later the player shows up in Music Assistant.

Back to stock for one boot:  unplug the stick, power-cycle.
Remove completely:           sh /media/sda1/native/uninstall.sh ; reboot
Status / logs:               sh /media/sda1/native/diag.sh
                             native/log.txt, native/sendspin.log, native/wpa.log
Audio test (native mode):    sh /media/sda1/native/selftest.sh

Not affiliated with the speaker's manufacturer. Use at your own risk.
