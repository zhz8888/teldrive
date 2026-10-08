{ lib, buildGoModule, version, commit, buildDate, uiDist
, # Proxy the Go module cache is fetched through. The default matches the
  # maintainer's own `go env GOPROXY`, because proxy.golang.org is unreachable
  # from this fork's usual network; override it (for example with
  # `teldrive.override { goProxy = "https://proxy.golang.org,direct"; }`) on a
  # network where the default is slow or blocked. The vendorHash below is
  # independent of the proxy, since the module contents are checksum-verified.
  goProxy ? "https://goproxy.cn,direct"
}:
buildGoModule {
  pname = "teldrive";
  inherit version;
  src = lib.cleanSourceWith {
    src = ../.;
    # filter keeps the store copy to the Go sources: the build output, the
    # JavaScript dependency trees and the local database artifacts would only
    # invalidate the derivation without being read.
    filter = path: _type:
      !(builtins.elem (baseNameOf path) [
        ".git"
        "bin"
        "result"
        "dist"
        "node_modules"
        "test-results"
      ]);
  };
  vendorHash = "sha256-ZkuDsxruypSE5wGWJCxGvhb1nQsFPO+cz1NqXcADDEY=";
  subPackages = [ "cmd/teldrive" ];
  env = {
    CGO_ENABLED = "0";
    GOPROXY = goProxy;
  };
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
    "-X main.commit=${commit}"
    "-X main.date=${buildDate}"
  ];
  preBuild = ''
    cp -r ${uiDist} ui/dist
    chmod -R u+w ui/dist
  '';
  # buildGoModule declares GOPROXY as an impure environment variable of the
  # module-fetching derivation, and Nix then discards the derivation's own
  # value in favour of the (empty) one from the daemon's environment, so
  # `go mod vendor` silently falls back to proxy.golang.org. Dropping it from
  # that list is what lets goProxy above take effect.
  overrideModAttrs = _finalAttrs: prevAttrs: {
    impureEnvVars = builtins.filter (v: v != "GOPROXY") (prevAttrs.impureEnvVars or [ ]);
  };
  doCheck = false;
}
