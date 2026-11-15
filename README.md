# sendspin-shelby

A native [Sendspin](https://github.com/Sendspin) player for the Bose SoundTouch 10 speaker, so it can play
synchronised audio from [Music Assistant](https://music-assistant.io) again: no cloud, and no vendor
software in the audio path.

It runs on the speaker's **stock firmware**. A boot hook keeps the vendor audio stack from starting,
and a static build of [sendspin-go](https://github.com/Sendspin/sendspin-go) with its own output
backend plays straight into the speaker's PCM device (`/dev/snd/snd_shelby`, hence the name). You
keep the speaker's own sound tuning: the backend drives the codec's DSP the same way the vendor
firmware does.

Nothing is flashed. The only change on the speaker is one script on its writable partition, and pulling
the USB stick brings back the stock firmware.

> **Status:** verified on one SoundTouch 10 (`/proc/variant` = `rhino`, kernel 3.14.43). Other
> models of the family use different hardware variants and are untested; `install.sh` refuses them
> unless you pass `--force`.

## What you need

- The speaker, a **FAT32 USB stick** and a **micro-USB OTG adapter** for the speaker's service port.
- **Music Assistant** with the *Sendspin* provider enabled, reachable from the speaker's network on
  **TCP port 8927**. The player connects *to* the server.
- A **WPA2-PSK** Wi-Fi network for the speaker.
- A computer with `ssh`.

## Install

1. **Download** `sendspin-shelby-stick-<version>.zip` from the
   [releases](https://github.com/hajoha/sendspin-shelby/releases) and unpack it onto the **root** of the
   FAT32 stick. Afterwards the stick contains `README.txt`, `remote_services` and `native/`.

2. **Configure** on your computer, in `native/` on the stick:
   ```sh
   cp wpa.conf.example wpa.conf          # set ssid and psk
   cp player.yaml.example player.yaml    # set name (and server, see below)
   echo DE > country                     # optional: your Wi-Fi country code
   ```

3. **Boot the speaker with the stick.** Plug the stick into the micro-USB port with the OTG adapter,
   then cut the power for 10 s. With `remote_services` on the stick, the stock firmware starts ssh
   and telnet for the root user without a password. They stay up only while that stick is plugged in.

4. **Log in and install.** Find the speaker's IP in your router, then:
   ```sh
   ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedKeyTypes=+ssh-rsa root@SPEAKER_IP
   sh /media/sda1/native/backup.sh    # once: full copy of the flash onto the stick
   sh /media/sda1/native/install.sh   # checks everything, installs the boot hook
   reboot
   ```
   Copy the backup folder (`/media/sda1/flash-backup-*`) off the stick and keep it.

5. After about 2 minutes the player shows up in Music Assistant under the name from `player.yaml`.
   Start at a **low volume**.

## Getting back to stock

| Goal | How |
|---|---|
| Stock firmware for one boot | Unplug the stick, power-cycle. Without the `native-mode` flag the hook does nothing. |
| Native mode never got onto Wi-Fi | Nothing to do: after 90 s without an IP, `start.sh` starts the vendor stack instead. |
| Remove completely | `sh /media/sda1/native/uninstall.sh`, then `reboot`. |

## Configuration

All files live in `native/` on the stick.

| File | Purpose |
|---|---|
| `wpa.conf` | Wi-Fi (wpa_supplicant syntax). Required. |
| `player.yaml` | sendspin-go config: `name`, `server`, `static_delay_ms`, ... `audio_device: /dev/snd/snd_shelby` is required. |
| `country` | Optional Wi-Fi regulatory domain, e.g. `DE`. |
| `hostname` | Optional DHCP hostname (default `sendspin-shelby`). |
| `max_db` | Optional loudness at volume 100 %, in dB. `dsp` mode: default `-40`, never above `-20`. `passthrough` mode: default `-24`. |
| `codec_mode` | Optional. `dsp` (default) uses the speaker's own sound tuning; `passthrough` uses an untuned codec path with software volume. |

After editing `player.yaml`, `max_db` or `codec_mode`, restart the player with `killall sendspin-go`
(it restarts after 5 s). For `codec_mode`, reboot instead.

**Server address:** the player finds Music Assistant via mDNS and then connects to the address the
server advertises. If your server sits on several networks (VLANs) and `native/sendspin.log` shows
`connection refused` or timeouts, set `server: <address the speaker can reach>:8927` in
`player.yaml`.

## Troubleshooting

```sh
sh /media/sda1/native/diag.sh       # status: mode, network, codec, DMA, player log
sh /media/sda1/native/selftest.sh   # native mode: 6 s test tone without the player, then reboot
```

Logs on the stick, readable on any computer: `native/log.txt` (boot), `native/wpa.log` (Wi-Fi),
`native/sendspin.log` (player), and `/mnt/nv/native-boot.log` on the speaker (boot hook).

- **No player in Music Assistant:** check `sendspin.log` for `Connected to server`. See *Server
  address* above, and that port 8927 is open on the server.
- **No Wi-Fi in native mode:** the speaker falls back to stock after 90 s. Check the SSID spelling
  (case-sensitive) and `wpa.log`.
- **Silence, but the player is connected:** `diag.sh` should show about 93 DMA buffers/s. If it shows
  0, reboot (see *How it works*).

## How it works

- **Boot hook.** The stock init runs `/mnt/nv/rc.local` (on the writable flash partition) at `S97`,
  before the vendor stack starts at `S99`. If the stick holds `native-mode`, the hook bind-mounts a
  no-op over `/etc/init.d/SoundTouch` (in RAM only) and starts `native/start.sh`. That script brings
  up Wi-Fi itself, configures the codec and runs the player in a restart loop.
- **PCM device.** `/dev/snd/snd_shelby` takes 48 kHz stereo s32le, left-justified, in writes of
  **exactly 4096 bytes** (512 frames). The driver copies each write into a DMA buffer without a
  length check. It must be opened **`O_RDWR`**: a write-only open runs the driver's TX reset path,
  after which DMA stays stopped until the next reboot. The hardware clock paces the writes.
- **Sound tuning and volume.** In `dsp` mode the codec runs the vendor's DSP program
  (`rhino_sm2_normal.cfg`, already on the speaker). It loads with the volume coefficient at 0, i.e.
  silent. Like the vendor firmware, the backend writes the volume as 8 coefficients to page 44,
  registers 16..47 of the TLV320AIC3256 (`C0 = 0.5·10^(dB/20)`, `C1 = dB/128`, Q1.23), into both
  adaptive buffers: write, flip, write. Samples go out unscaled. The backend also mutes the amplifier
  after 3 s of silence.
- **Player.** sendspin-go **1.8.2** (1.8.0 ignores the server's volume commands) plus
  [`player/shelby.go`](player/shelby.go), cross-compiled as a fully static armv7 musl binary so it
  doesn't depend on the speaker's 2017 userland.

## Building

```sh
nix build .#stick     # result/sendspin-shelby-stick-<version>.zip, result/stick/
nix build .#player    # result/bin/sendspin-go only
nix flake check       # backend unit tests, script syntax check, full build
```

CI (`.github/workflows/build.yml`) runs the same on every push and attaches the zip to a GitHub
release for `v*` tags. A tag must match `version` in `flake.nix`.

## Disclaimer

Use at your own risk. This modifies how your speaker boots and can, in the worst case, leave it
needing recovery. Make the flash backup first.

Bose and SoundTouch are trademarks of Bose Corporation. This project is not affiliated with,
endorsed by, or sponsored by Bose Corporation; the names are used only to describe which hardware
it runs on. The release contains no vendor files. See [NOTICE](NOTICE).

## License

[Apache-2.0](LICENSE), the same as sendspin-go.
