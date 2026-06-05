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
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "rig";
          version = "0.0.1";
          src = ./.;
          vendorHash = "sha256-76yvZ8MhGnJhkfBALS/MYAeEmkOcLX0VKu1zTtPLhxo=";
          meta.mainProgram = "rig";
        };

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
