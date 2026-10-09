{
  description = "Teldrive server binary";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs, ... }:
    let
      # The published binaries are Linux, but the devShell is also how a macOS
      # checkout gets go/bun/sqlc at the versions the justfile pins, so Darwin is
      # built here as well. It is aarch64 only: this flake tracks
      # nixos-unstable, and nixpkgs dropped x86_64-darwin in 26.11.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      version = "dev";

      # mkPkgs is the nixpkgs instance every output evaluates against.
      mkPkgs = system: import nixpkgs {
        inherit system;
      };
    in {
      packages = forAllSystems (system:
        let
          pkgs = mkPkgs system;

          commit = self.shortRev or self.dirtyShortRev or "unknown";
          buildDate = self.lastModifiedDate or "unknown";

          uiDist = pkgs.callPackage ./nix/ui.nix { inherit version; };
          teldrive = pkgs.callPackage ./nix/package.nix {
            inherit version commit buildDate uiDist;
          };
        in {
          teldrive = teldrive;
          default = teldrive;
        });

      # Installing this overlay is enough for the modules below: they default to
      # `pkgs.teldrive`, and nix/ui.nix builds the UI from the fixed-output
      # node_modules of that same package set.
      overlays.default = final: prev: {
        teldrive-ui-dist = final.callPackage ./nix/ui.nix { inherit version; };
        teldrive = final.callPackage ./nix/package.nix {
          inherit version;
          commit = "unknown";
          buildDate = "unknown";
          uiDist = final.teldrive-ui-dist;
        };
      };

      nixosModules.default = import ./nix/modules/nixos.nix;
      homeManagerModules.default = import ./nix/modules/home.nix;

      devShells = forAllSystems (system:
        let
          pkgs = mkPkgs system;
        in {
          # Toolchain for the just workflows: go/bun/nodejs run the code,
          # sqlc + patchsqlc regenerate the DB layer (must be v1.31.1),
          # just drives justfile, podman backs integration tests, and
          # postgresql provides psql for debugging test databases.
          #
          # nixpkgs only packages Chromium for Linux, so the Darwin shells use
          # the browser Playwright downloads itself instead (the UI's
          # playwright.config.ts already treats the executable path as an
          # optional override).
          default = pkgs.mkShell (
            {
              packages = [
                pkgs.go
                pkgs.bun
                pkgs.nodejs
                pkgs.ffmpeg
                pkgs.sqlc
                pkgs.just
                pkgs.git
                pkgs.podman
                pkgs.postgresql
              ] ++ nixpkgs.lib.optional pkgs.stdenv.hostPlatform.isLinux pkgs.chromium;
              shellHook = ''
                echo "teldrive dev shell: $(go version | cut -d' ' -f3), bun $(bun --version), sqlc $(sqlc version 2>/dev/null | head -n1)"
                echo "run 'just --list' for workflows (try: just install-tools)"
              '';
            }
            // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
              PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH = "${pkgs.chromium}/bin/chromium";
              PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD = "1";
            }
          );
        });
    };
}
