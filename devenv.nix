# BONNIE development shell. Enter it with `devenv shell`, or with
# `direnv allow` once (see .envrc).
{
  pkgs,
  lib,
  config,
  ...
}:

let
  # The shell uses the working tree's recipe, not the pinned release's, so
  # master can move the microsandbox runtime first. flake.nix installs the
  # release's recipe for users.
  microsandbox = pkgs.callPackage ./nix/microsandbox.nix { };
in
{
  packages = [
    # Toolchain. BONNIE targets the Go version in go.mod, which is not yet
    # the nixpkgs default.
    pkgs.go_1_27
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

    # Keep Go's caches inside the repository, so `devenv shell` and a direnv
    # shell agree, and `git clean -xdf` really cleans. Go's `./...` skips
    # directories that start with `.`; a raw file walk does not (AGENTS.md).
    GOPATH = "${config.devenv.root}/.direnv/go";
    GOBIN = "${config.devenv.root}/.direnv/go/bin";

    # Pure-Go FFI packages (jupiterrider/ffi, purego) dlopen these at run
    # time; Nix-built binaries only search RUNPATH and LD_LIBRARY_PATH. On
    # Linux, examples/self-contained-agent also loads Kronk's prebuilt
    # llama.cpp, which links libstdc++.
    LD_LIBRARY_PATH = lib.makeLibraryPath (
      [ pkgs.libffi ] ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.stdenv.cc.cc.lib ]
    );
  };

  enterShell = ''
    export PATH="$GOBIN:$PATH"

    # Stay quiet for `devenv shell -- <command>` and other non-interactive use.
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
}
