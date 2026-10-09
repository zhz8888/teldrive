{ lib, stdenv, bun, nodejs, version }:
let
  src = lib.cleanSourceWith {
    src = ../ui;
    # filter keeps the source tree only: node_modules comes from the
    # fixed-output derivation below and dist is what this derivation builds.
    filter = path: _type:
      !(builtins.elem (baseNameOf path) [ "node_modules" "dist" "test-results" ]);
  };
  # Lockfiles only, so UI source edits don't invalidate the dep hash.
  depsSrc = lib.fileset.toSource {
    root = ../ui;
    fileset = lib.fileset.unions [
      ../ui/package.json
      ../ui/bun.lock
    ];
  };

  system = stdenv.hostPlatform.system;

  # ui/bun.lock pins per-OS and per-CPU binaries (@biomejs/cli-*,
  # @rolldown/binding-*, @napi-rs/canvas-*, fsevents), and
  # `bun install --frozen-lockfile` only materializes the ones matching the
  # build platform, so every system needs its own fixed-output hash.
  # `just update-ui-deps-hash` re-pins the entry for the machine it runs on.
  # lib.fakeSha256 marks a platform that is not pinned yet: the build then
  # stops in the dependency install and reports the hash that belongs here.
  uiDepsHashes = {
    "aarch64-darwin" = "sha256-licOL9oWuAxbwD3/SXVicO6TVPWoqvvZ0YH5XAbCGJY=";
    "aarch64-linux" = "sha256-03X8DRToSTXqOjF6L88L9Pw1duMy6tjHWnIqey5oP0U=";
    "x86_64-linux" = "sha256-yaA4AI2npsMjfIQk0xHEA7JYlsFX/Y8wxl/6astH0qI=";
  };

  bunDeps = stdenv.mkDerivation {
    pname = "teldrive-ui-node_modules";
    inherit version;
    src = depsSrc;
    nativeBuildInputs = [ bun nodejs ];
    # Fixed-output: network allowed here to fetch the lockfile closure.
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = uiDepsHashes.${system} or (throw ''
      nix/ui.nix has no UI dependency hash for ${system}.
      Add that system to uiDepsHashes and run `just update-ui-deps-hash` on it.
    '');
    # FOD outputs must not reference /nix/store. Keep vendored files
    # byte-identical to upstream: no shebang rewrites, no ELF patching
    # of prebuilt NAPI binaries (tailwind oxide, rolldown, biome).
    dontPatchShebangs = true;
    dontPatchELF = true;
    buildPhase = ''
      runHook preBuild
      export HOME=$TMPDIR
      export BUN_INSTALL_CACHE=$TMPDIR/bun-cache
      bun install --frozen-lockfile --backend=copyfile
      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      cp -r node_modules $out
      runHook postInstall
    '';
  };
in
stdenv.mkDerivation {
  pname = "teldrive-ui-dist";
  inherit version;
  inherit src;
  nativeBuildInputs = [ bun nodejs ];
  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR
    cp -r ${bunDeps} node_modules
    chmod -R u+w node_modules
    patchShebangs node_modules
    bun run --frozen-lockfile build
    runHook postBuild
  '';
  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -r dist/. $out/
    runHook postInstall
  '';
}
