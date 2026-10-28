# The BONNIE CLI, built from the release that flake.nix pins.
{
  lib,
  buildGoModule,
  makeWrapper,
  microsandbox,
  # The source tree of the pinned release (inputs.bonnie-release), and its
  # version without the "v".
  src,
  version,
}:

buildGoModule {
  pname = "bonnie";
  inherit src version;

  # Fetch the modules in a fixed-output derivation and vendor them for the
  # build. The hash pins the module tree of the pinned release, so it has to
  # change with every release that ships different dependencies. The
  # `nix-release-pin` job refreshes it after each release; refresh it by hand
  # with:
  #
  #   scripts/update-flake-vendor-hash.sh
  vendorHash = "sha256-i0EyO1XBpUKCmBaHI6nl3nnctNHUFz69HL6WZpXecZ8=";

  subPackages = [ "cmd/bonnie" ];

  # CI tested this release before it was tagged. The Nix sandbox also has no
  # network, and several cmd/bonnie tests build a separate agent module.
  doCheck = false;

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  nativeBuildInputs = [ makeWrapper ];

  # BONNIE drives the microsandbox CLI as a subprocess, so `msb` must be on
  # PATH. Suffix, not prefix: a user-installed msb still wins.
  postInstall = ''
    wrapProgram "$out/bin/bonnie" \
      --suffix PATH : ${lib.makeBinPath [ microsandbox ]}
  '';

  meta = {
    description = "Durable, resumable agent runs on the Kit SDK";
    homepage = "https://github.com/mark3labs/bonnie";
    license = lib.licenses.mit;
    mainProgram = "bonnie";
    platforms = lib.platforms.linux;
  };
}
