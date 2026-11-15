# The release artifact: everything that goes onto the USB stick, as a folder
# and as a zip. Unzip onto the root of a FAT32 stick.
#
#   remote_services   empty file; with it on the stick the stock firmware
#                     starts ssh and telnet (how you get in to install)
#   README.txt        short install guide
#   native/           player binary, boot hook, scripts, config examples
{
  runCommand,
  python3,
  zip,
  player,
  version,
}:

runCommand "sendspin-shelby-stick-${version}"
  {
    nativeBuildInputs = [
      python3
      zip
    ];
  }
  ''
    root=$PWD/stick
    mkdir -p $root/native

    install -m 755 ${player}/bin/sendspin-go $root/native/sendspin-go
    for f in ${../stick}/*; do
      case "$f" in
        *.example) install -m 644 "$f" $root/native/ ;;
        *) install -m 755 "$f" $root/native/ ;;
      esac
    done
    python3 ${../tools/mktone.py} $root/native/tone-20dB.raw
    echo "sendspin-shelby ${version} (sendspin-go ${player.version})" > $root/native/VERSION
    install -m 644 ${../docs/STICK-README.txt} $root/README.txt
    : > $root/remote_services

    # Reproducible zip: fixed timestamps, no extra attributes, sorted names.
    find $root -exec touch -h -d @315532800 {} +
    mkdir -p $out
    (cd $root && find . -type f | sort | zip -X -9 -@ $out/sendspin-shelby-stick-${version}.zip)
    cp -r $root $out/stick
    (cd $out && sha256sum *.zip > SHA256SUMS)
  ''
