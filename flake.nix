{
  description = "WhatsApp TUI development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            golangci-lint
            sqlite
            pkg-config
            gcc
          ];

          shellHook = ''
            export CGO_ENABLED=1
            echo "watui development shell loaded (Go $(go version | awk '{print $3}'))"
          '';
        };
      });
}
