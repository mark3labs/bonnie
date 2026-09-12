# The BONNIE CLI.
{
  lib,
  buildGoModule,
  makeWrapper,
  microsandbox,
}:

buildGoModule (finalAttrs: {
  pname = "bonnie";
  version = "0.1.0";

  src = lib.cleanSourceWith {
    src = ../.;
    filter =
      path: type:
      let
        base = baseNameOf path;
      in
      !(builtins.elem base [
        ".github"
        ".kit"
        ".bonnie"
        "docs"
        "examples"
        "result"
      ]);
  };

  # Refresh with:
  #   nix build .#bonnie.goModules --rebuild
  # or set this to lib.fakeHash and read the hash Nix prints.
  vendorHash = "sha256-qFBVkgLgsZSYvqB2Qx6D8WRAhXe3+B9mOShEFRvcoqQ=";

  subPackages = [ "cmd/bonnie" ];

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}"
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
  };
})
