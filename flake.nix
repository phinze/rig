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
        rig = pkgs.buildGoModule {
          pname = "rig";
          version = "0.0.1";
          src = ./.;
          vendorHash = "sha256-i8UEIG6MuO3miZs4LikT9C7CA33wX0bqPd6ynjoAYjc=";
          meta.mainProgram = "rig";
        };
      in
      {
        packages.default = rig;

        # Keep the fixed-output dependency hash honest. `go test` resolves
        # modules directly and cannot catch a stale buildGoModule vendorHash.
        checks.default = rig;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            golangci-lint
            gnumake
          ];
        };
      }
    );
}
