{
  description = "sendspin-shelby: native Sendspin player for speakers on the snd_shelby (AM335x) platform";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      version = "0.1.0";
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      # Static armv7 (musl): runs on the speaker's stock kernel 3.14 / glibc 2.15.
      cross = pkgs.pkgsCross.armv7l-hf-multiplatform.pkgsStatic;
      player = cross.callPackage ./nix/player.nix { };
    in
    {
      packages.${system} = {
        # result/bin/sendspin-go
        inherit player;
        # result/sendspin-shelby-stick-<version>.zip + result/stick/
        stick = pkgs.callPackage ./nix/stick.nix { inherit player version; };
        default = self.packages.${system}.stick;
      };

      checks.${system} = {
        # Unit tests of the snd_shelby backend (native build).
        backend-tests = pkgs.callPackage ./nix/player.nix { runTests = true; };

        # The scripts run under the speaker's busybox ash: syntax-check them there.
        stick-scripts = pkgs.runCommand "stick-scripts-syntax" { nativeBuildInputs = [ pkgs.busybox ]; } ''
          for f in ${./stick}/*.sh ${./stick}/rc.local; do
            echo "busybox sh -n $(basename $f)"
            busybox sh -n "$f"
          done
          touch $out
        '';

        stick = self.packages.${system}.stick;
      };

      formatter.${system} = pkgs.nixfmt;
    };
}
