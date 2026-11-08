{
  description = "BONNIE — durable, resumable agent runs on the Kit SDK";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # The release that `nix profile add github:mark3labs/bonnie` installs.
    #
    # A flake input cannot say "latest tag". A bare `github:mark3labs/bonnie`
    # resolves to the default branch, so an unpinned input would install
    # unreleased master instead of a release. The `nix-release-pin` job in
    # .github/workflows/release.yml rewrites the url below after every tagged
    # release, so the pin on the default branch always names the newest tag.
    #
    # Keep the `# nix:bonnie-release-tag` marker on the url line:
    # scripts/bump-flake-release-pin.sh matches on it.
    bonnie-release = {
      url = "github:mark3labs/bonnie/v0.38.0"; # nix:bonnie-release-tag
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      bonnie-release,
    }:
    let
      # Version of the release pinned by inputs.bonnie-release above, without
      # the "v". scripts/bump-flake-release-pin.sh writes both lines from one
      # tag and refuses to run when they disagree.
      #
      # It cannot be derived from the input: a `flake = false` input arrives in
      # the outputs as a bare store path, so the tag in the url is not visible
      # here.
      version = "0.38.0"; # nix:bonnie-release-version

      # microsandbox ships releases for these three targets only.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # go.mod asks for Go 1.27, which is not yet the nixpkgs default.
      goPackage = pkgs: pkgs.go_1_27;

      # What a user installs comes from the pinned release, also `msb`: the
      # released CLI needs the microsandbox runtime its own installer pins
      # (scripts/microsandbox_pin_test.go). The dev shell (devenv.nix) uses
      # the working tree's recipe instead, so master can move the runtime first.
      releaseMicrosandbox = pkgs: pkgs.callPackage (bonnie-release + "/nix/microsandbox.nix") { };
    in
    {
      overlays.default = final: prev: {
        microsandbox = releaseMicrosandbox final;
        bonnie = final.callPackage ./nix/bonnie.nix {
          inherit (final) microsandbox;
          inherit version;
          src = bonnie-release;
          buildGoModule = final.buildGoModule.override { go = goPackage final; };
        };
      };

      packages = forAllSystems (
        pkgs:
        let
          microsandbox = releaseMicrosandbox pkgs;
          bonnie = pkgs.callPackage ./nix/bonnie.nix {
            inherit microsandbox version;
            src = bonnie-release;
            buildGoModule = pkgs.buildGoModule.override { go = goPackage pkgs; };
          };
        in
        {
          inherit microsandbox;
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          inherit bonnie;
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
          msb = {
            type = "app";
            program = "${self.packages.${system}.microsandbox}/bin/msb";
            meta = self.packages.${system}.microsandbox.meta;
          };
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          default = bonnieApp;
          bonnie = bonnieApp;
        }
      );

      # `nix flake check` builds each package of the system.
      checks = forAllSystems (
        pkgs:
        let
          inherit (pkgs.stdenv.hostPlatform) system isLinux;
        in
        {
          inherit (self.packages.${system}) microsandbox;
        }
        // nixpkgs.lib.optionalAttrs isLinux {
          inherit (self.packages.${system}) bonnie;
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
