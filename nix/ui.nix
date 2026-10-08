{ lib, stdenv, bun2nix, bun, nodejs, version }:
stdenv.mkDerivation {
  pname = "teldrive-ui-dist";
  inherit version;
  src = lib.cleanSourceWith {
    src = ../ui;
    filter = path: _type:
      !(builtins.elem (baseNameOf path) [ "node_modules" "dist" "test-results" ]);
  };
  nativeBuildInputs = [ bun2nix.hook bun nodejs ];
  bunDeps = bun2nix.fetchBunDeps {
    bunNix = ../ui/bun.nix;
  };
  # The hook copies the pre-fetched bun cache out of the store, which is
  # read-only, but bun 1.4 still writes lookup entries next to scoped packages
  # (`@scope/name/<version>@@<registry>@@@1`) while installing. Without this
  # every scoped dependency fails with
  # `moving "@scope/name" to cache dir failed: EACCES ... (rename())`
  # and `bun install` then tries to re-download it, which the sandbox blocks.
  postBunSetInstallCacheDirPhase = ''
    chmod -R u+w "$BUN_INSTALL_CACHE_DIR"
  '';
  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR
    bun run build
    runHook postBuild
  '';
  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -r dist/. $out/
    runHook postInstall
  '';
}
