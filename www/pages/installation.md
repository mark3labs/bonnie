---
title: Installation
description: Install the BONNIE CLI, prepare a Linux host, and configure a model provider.
---

## Requirements

| Requirement | When you need it |
| --- | --- |
| Linux amd64 or arm64 | Supported BONNIE host |
| Linux 5.13 or newer, with Landlock enabled | Default sandbox |
| Go 1.27 or newer | Authoring and compiling agent trees; source installation |
| Provider credentials | Model calls |
| Docker or microsandbox | Only when you select that backend |

macOS and Windows are not supported BONNIE hosts. A compiled agent does not need Go or the BONNIE CLI, but its selected sandbox still needs its runtime prerequisites.

## Install a release binary

Download a Linux archive and its checksum file from the same [GitHub release](https://github.com/mark3labs/bonnie/releases). Verify the archive before you extract or execute it. Install only the `bonnie` executable. A checksum confirms agreement with the published manifest; it is not an independent publisher signature or proof of safety.

This Bash example uses `v0.35.0` for Linux amd64. For Linux arm64, set `arch=arm64`. To use another published tag, select it from Releases and change `version`; the archive and checksum file must use that same version. Required tools are `bash`, `curl`, `sha256sum`, `awk`, `tar`, `install`, and `mktemp`, plus the standard `mkdir` and `rm` commands.

```bash
(
  set -eu
  version=0.35.0
  arch=amd64
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  cd "$work"
  archive="bonnie_${version}_linux_${arch}.tar.gz"
  sums="bonnie_${version}_checksums.txt"
  base="https://github.com/mark3labs/bonnie/releases/download/v${version}"
  curl -fsSL "$base/$archive" -o "$archive"
  curl -fsSL "$base/$sums" -o "$sums"
  awk -v archive="$archive" '
    $2 == archive { count++; entry = $0 }
    END {
      if (count != 1) exit 1
      print entry
    }
  ' "$sums" > selected-checksum.txt
  sha256sum --check selected-checksum.txt
  tar -xzf "$archive" bonnie
  mkdir -p "$HOME/.local/bin"
  install -m 0755 bonnie "$HOME/.local/bin/bonnie"
)
```

Ensure `~/.local/bin` is in `PATH`, then check the installed CLI:

```bash
bonnie version
```

### Optional: download and review the installer

The [installer source](https://github.com/mark3labs/bonnie/blob/master/install.sh) downloads a release and refuses a binary whose SHA-256 checksum it cannot verify. Read it before execution. It selects the first existing writable directory from `~/.local/bin`, `~/bin`, and `/usr/local/bin`, or creates `~/.local/bin` if none qualifies. It does not configure Go, credentials, or sandbox prerequisites.

Download the script to a local file:

```bash
curl -fsSLO https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh
```

Inspect it with `less` or an editor:

```bash
less install.sh
```

Execute only after review:

```bash
bash install.sh
```

The download URL tracks `master`. The `--version` option selects the binary release, not the script revision. To choose a release and destination after review:

```bash
bash install.sh --version v0.35.0 --bin-dir "$HOME/.local/bin"
```

## Install from source

```bash
go install github.com/mark3labs/bonnie/cmd/bonnie@latest
```

Put `$(go env GOPATH)/bin` in `PATH` if it is not there already.

To use the library without installing the CLI:

```bash
go get github.com/mark3labs/bonnie
```

## Install with Nix

```bash
nix profile add github:mark3labs/bonnie
# Or run without a profile installation:
nix run github:mark3labs/bonnie -- version
```

The flake supplies BONNIE and the microsandbox CLI. In a source checkout, `nix develop` supplies the development tools. `direnv allow` loads the same shell through `.envrc`.

## Configure a provider

```bash
export ANTHROPIC_API_KEY=your-provider-key
# Alternatives:
# export OPENAI_API_KEY=your-provider-key
# export GEMINI_API_KEY=your-provider-key
```

Select a model with `bonnie init --model`, `bonnie.WithModel`, or the operator's `--model` flag. Provider and model support comes from [Kit](https://go-kit.dev/providers).

BONNIE reads `.env` from its working directory. An exported environment variable wins over a value in that file. A missing `.env` is not an error.

```dotenv
ANTHROPIC_API_KEY=your-provider-key
```

Do not commit credential files. Do not put credentials in `context/`, skills, prompts, or the sandbox environment. The host needs the model key; sandbox commands do not.

### Provider login

For OpenAI ChatGPT/Codex subscription OAuth instead of an API key:

```bash
bonnie auth login openai
bonnie auth status
bonnie auth logout openai
```

Compiled agents include the same commands by default. Credentials are shared
with Kit. This is not general OpenAI API OAuth, HTTP bearer authentication, or
MCP authentication. Login prints a browser URL. The browser must reach
`localhost:1455` on the login host; forward loopback port 1455 for a remote
host. There is no manual code-entry fallback, and login ends after at most
five minutes. Status reports stored credentials only, not environment keys or
token validity. Logout removes stored credentials, not environment variables.
See [CLI](/reference/cli#provider-login).

## Check the installation

```bash
bonnie version
bonnie --help
```

Continue with [Quick start](/quick-start). If startup reports a sandbox error, read [Sandboxes](/guides/sandboxes) and [Troubleshooting](/guides/troubleshooting). BONNIE refuses a default sandbox that cannot confine tool execution; it does not silently select host execution.
