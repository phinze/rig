{
  description = "rig: workspace tool for task-shaped multi-repo work";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        # Pinned rather than taking pkgs.go, which still tracks 1.26 in
        # nixpkgs. go.mod names the same version, so the toolchain a devShell
        # gets and the one the package builds with cannot drift apart.
        go = pkgs.go_1_27;
        rig = (pkgs.buildGoModule.override { inherit go; }) {
          pname = "rig";
          version = "0.0.1";
          src = ./.;
          vendorHash = "sha256-Myhws+jwLQE6N5gv41E5BlQJ1dXHG3gydEi8VVYShWs=";
          meta.mainProgram = "rig";
        };
      in
      {
        packages.default = rig;

        # Keep the fixed-output dependency hash honest. `go test` resolves
        # modules directly and cannot catch a stale buildGoModule vendorHash.
        checks.default = rig;

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.golangci-lint
            pkgs.gnumake
          ];
        };
      }
    );
}
