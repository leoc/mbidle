{
  description = "mbidle: IMAP IDLE/scan watcher that triggers targeted mbsync runs";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "mbidle";
          version = "2.0.0";
          src = pkgs.lib.cleanSourceWith {
            src = ./.;
            filter = path: type: !(pkgs.lib.hasInfix "mbidle-legacy" path);
          };
          vendorHash = "sha256-P06DKiHxZ+KM5hIzFIlELCK2jpnG6uDFm5uyNa4+mj4=";
          meta.mainProgram = "mbidle";
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls pkgs.isync ]; };
      });
    };
}
