{
  description = "BONNIE — durable, resumable agent runs on the Kit SDK";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      # microsandbox ships releases for these three targets only.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # go.mod asks for Go 1.27, which is not yet the nixpkgs default.
      goPackage = pkgs: pkgs.go_1_27;
    in
    {
      overlays.default = final: prev: {
        microsandbox = final.callPackage ./nix/microsandbox.nix { };
        bonnie = final.callPackage ./nix/bonnie.nix {
          inherit (final) microsandbox;
          buildGoModule = final.buildGoModule.override { go = goPackage final; };
        };
      };

      packages = forAllSystems (
        pkgs:
        let
          microsandbox = pkgs.callPackage ./nix/microsandbox.nix { };
          bonnie = pkgs.callPackage ./nix/bonnie.nix {
            inherit microsandbox;
            buildGoModule = pkgs.buildGoModule.override { go = goPackage pkgs; };
          };
        in
        {
          inherit bonnie microsandbox;
          default = bonnie;
        }
      );

      apps = forAllSystems (
        pkgs:
        let
          inherit (pkgs.stdenv.hostPlatform) system;
          bonnieApp = {
            type = "app";
            program = "${self.packages.${system}.bonnie}/bin/bonnie";
            meta = self.packages.${system}.bonnie.meta;
          };
        in
        {
          default = bonnieApp;
          bonnie = bonnieApp;
          msb = {
            type = "app";
            program = "${self.packages.${system}.microsandbox}/bin/msb";
            meta = self.packages.${system}.microsandbox.meta;
          };
        }
      );

      # `nix flake check` builds both packages.
      checks = forAllSystems (
        pkgs:
        let
          inherit (pkgs.stdenv.hostPlatform) system;
        in
        {
          inherit (self.packages.${system}) bonnie microsandbox;
        }
      );

      devShells = forAllSystems (
        pkgs:
        let
          microsandbox = self.packages.${pkgs.stdenv.hostPlatform.system}.microsandbox;
        in
        {
          default = pkgs.mkShell {
            name = "bonnie";

            packages = [
              # Toolchain. BONNIE targets the Go version in go.mod.
              (goPackage pkgs)
              pkgs.gopls # language server
              pkgs.gotools # goimports
              pkgs.go-tools # staticcheck
              pkgs.delve # debugger

              # The commands AGENTS.md documents.
              pkgs.golangci-lint
              pkgs.goreleaser
              pkgs.go-task # task lint, task dev -- serve, ...

              # The sandbox backend BONNIE drives as a subprocess.
              microsandbox
            ];

            env = {
              # Use the Go from this shell. Never download a toolchain.
              GOTOOLCHAIN = "local";
            };

            shellHook = ''
              # Stay quiet for `nix develop --command ...`, which CI uses.
              if [ -n "''${PS1-}" ]; then
                echo "BONNIE dev shell"
                echo "  go          $(go version | cut -d' ' -f3)"
                echo "  msb         $(msb --version 2>/dev/null || echo 'not runnable on this host')"
                echo
                echo "  build   go build ./..."
                echo "  test    go test -race ./..."
                echo "  lint    golangci-lint run"
              fi
            '';
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
