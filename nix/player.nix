# sendspin-go player with the snd_shelby output backend.
#
# Built via pkgsCross.armv7l-hf-multiplatform.pkgsStatic.callPackage (see
# flake.nix): a fully static armv7 musl binary, so it runs on the speaker's
# stock firmware (kernel 3.14, glibc 2.15) without touching its libraries.
#
# With runTests = true (native callPackage) it instead builds only the
# output package and runs the backend's unit tests.
{
  lib,
  buildGoModule,
  buildPackages,
  fetchFromGitHub,
  libopus,
  pkg-config,
  runTests ? false,
}:

buildGoModule (finalAttrs: {
  pname = if runTests then "sendspin-shelby-backend-tests" else "sendspin-shelby-player";

  # 1.8.2, not 1.8.0: 1.8.0 drops the server's volume/mute commands
  # (nothing reads protocol.Client.ControlMsgs), fixed upstream in 449c52c.
  version = "1.8.2";

  src = fetchFromGitHub {
    owner = "Sendspin";
    repo = "sendspin-go";
    tag = "v${finalAttrs.version}";
    hash = "sha256-CRo5OrcrB6ih07DcN7ofFOMb1FwzyxqFM9ksboxp+VI=";
  };
  # go.mod/go.sum are identical to 1.8.0, so this is nixpkgs' 1.8.0 value.
  vendorHash = "sha256-QAmC6bgOSlV8we9j3rDQ9V3sLdSvELu8zzn5UAw/uIY=";

  patches = [ ../player/main-shelby-output.patch ];
  postPatch = ''
    cp ${../player/shelby.go} pkg/audio/output/shelby.go
  ''
  + lib.optionalString runTests ''
    cp ${../player/shelby_test.go} pkg/audio/output/shelby_test.go
  '';

  # Player only (the root main package); the server needs ffmpeg at runtime.
  subPackages = if runTests then [ "pkg/audio/output" ] else [ "." ];

  # hraban/opus without opusfile: only libopus is needed. miniaudio (malgo)
  # is compiled in but dlopen()s ALSA at runtime, which the snd_shelby
  # backend never uses.
  tags = [ "nolibopusfile" ];

  nativeBuildInputs = [ pkg-config ];
  buildInputs = lib.optionals (!runTests) [ libopus ];

  env = {
    CGO_ENABLED = "1";
    CGO_LDFLAGS = "-lm";
  };
  ldflags = [ "-s" ];

  doCheck = runTests;
  checkFlags = [
    "-run"
    "Shelby|LinearResampler|DSP"
  ];

  # The speaker has no /nix/store: refuse to produce anything but a static ELF.
  postFixup = lib.optionalString (!runTests) ''
    info=$(${buildPackages.file}/bin/file -b $out/bin/sendspin-go)
    echo "sendspin-go: $info"
    case "$info" in
      *ARM*statically\ linked*) ;;
      *) echo "sendspin-go is not a static ARM binary" >&2; exit 1 ;;
    esac
  '';

  meta = {
    description = "Sendspin player for the snd_shelby PCM device";
    homepage = "https://github.com/hajoha/sendspin-shelby";
    license = lib.licenses.asl20;
    mainProgram = "sendspin-go";
  };
})
