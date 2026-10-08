{
  description = "Teldrive server binary";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.bun2nix.url = "github:nix-community/bun2nix";
  inputs.bun2nix.inputs.nixpkgs.follows = "nixpkgs";
  inputs.nix-pkgs.url = "github:divyam234/nix-pkgs";
  inputs.nix-pkgs.inputs.nixpkgs.follows = "nixpkgs";

  outputs = { self, nixpkgs, bun2nix, ... }@inputs:
    let
      # The published binaries are Linux, but the devShell is also how a macOS
      # checkout gets go/bun/sqlc at the versions the justfile pins, so both
      # Darwin systems are built here as well.
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      version = "dev";

      # bun2nix's own package builds its Rust CLI from source, which is
      # uncached for most systems. divyam234/nix-pkgs ships prebuilt Linux
      # binaries; where one exists it replaces the CLI, and only the hook
      # script and the fetchBunDeps helper are kept from the bun2nix flake.
      # Darwin has no prebuilt binary, so it falls back to building the CLI.
      bun2nixPrebuilt = final: prev:
        let
          system = final.stdenv.hostPlatform.system;
          bun2nixPkg =
            if builtins.hasAttr system inputs."nix-pkgs".packages then
              inputs."nix-pkgs".packages.${system}.bun2nix
            else
              prev.bun2nix;

          slimHook = final.makeSetupHook
            {
              name = "bun2nix-hook";
              propagatedBuildInputs = [ final.bun final.yq-go ];
              substitutions = {
                resolveCatalogTs = inputs.bun2nix.outPath + "/nix/mk-derivation/resolve-catalog.ts";
                bunDefaultInstallFlags =
                  if final.stdenv.hostPlatform.isDarwin then
                    [
                      "--linker=isolated"
                      "--backend=symlink"
                    ]
                  else
                    [
                      "--linker=isolated"
                    ];
              };
            }
            (inputs.bun2nix.outPath + "/nix/mk-derivation/hook.sh");
        in
        {
          bun2nix = bun2nixPkg
            // {
              hook = slimHook;
              fetchBunDeps = prev.bun2nix.fetchBunDeps;
            };
        };

      # mkPkgs is the nixpkgs instance every output evaluates against: the
      # bun2nix from the flake input plus the prebuilt-binary overlay above, so
      # the JavaScript builds do not compile bun2nix's Rust CLI from source.
      mkPkgs = system: import nixpkgs {
        inherit system;
        overlays = [ bun2nix.overlays.default bun2nixPrebuilt ];
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
          bun2nix = pkgs.bun2nix;
        });

      # Composed with bun2nix's own overlay so that applying just this one
      # overlay is enough for the modules below: they default to `pkgs.teldrive`,
      # and nix/ui.nix builds the UI through bun2nix, which therefore has to be
      # in the package set as well.
      overlays.default = nixpkgs.lib.composeManyExtensions [
        bun2nix.overlays.default
        bun2nixPrebuilt
        (final: prev: {
          teldrive-ui-dist = final.callPackage ./nix/ui.nix { inherit version; };
          teldrive = final.callPackage ./nix/package.nix {
            inherit version;
            commit = "unknown";
            buildDate = "unknown";
            uiDist = final.teldrive-ui-dist;
          };
        })
      ];

      nixosModules.default = import ./nix/modules/nixos.nix;
      homeManagerModules.default = import ./nix/modules/home.nix;

      devShells = forAllSystems (system:
        let
          pkgs = mkPkgs system;
        in {
          # Toolchain for the just workflows: go/bun/nodejs run the code,
          # sqlc + patchsqlc regenerate the DB layer (must be v1.31.1),
          # just drives justfile, podman backs integration tests,
          # postgresql provides psql for debugging test databases, and
          # bun2nix regenerates ui/bun.nix through `just update-bun-nix`.
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
                pkgs.bun2nix
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
