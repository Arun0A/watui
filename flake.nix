{
  description = "WhatsApp TUI development environment and package";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        watui = pkgs.buildGoModule {
          pname = "watui";
          version = "0.1.0";
          src = ./.;

          subPackages = [ "cmd/watui" ];

          vendorHash = "sha256-9WDjkXwjEUylcBdAxinsycNOl9z4siG9AxmeoV9NYdQ=";

          env.CGO_ENABLED = 1;
          doCheck = false;

          nativeBuildInputs = with pkgs; [
            pkg-config
            gcc
          ];
        };
      in
      {
        packages.default = watui;
        apps.default = flake-utils.lib.mkApp {
          drv = watui;
        };

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
