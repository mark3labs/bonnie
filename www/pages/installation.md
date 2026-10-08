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

The installer downloads the platform binary, verifies its SHA-256 checksum, and installs it in `~/.local/bin` by default:

```bash
curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh | bash
```

Ensure `~/.local/bin` is in `PATH`. To choose a release and destination:

```bash
curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh \
  | bash -s -- --version v0.17.0 --bin-dir "$HOME/.local/bin"
```

Choose a tag from [Releases](https://github.com/mark3labs/bonnie/releases). Review the script before execution if your installation policy requires it. The installer reports missing Go, provider keys, or Landlock support; it does not configure the host for you.

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

## Check the installation

```bash
bonnie version
bonnie --help
```

Continue with [Quick start](/quick-start). If startup reports a sandbox error, read [Sandboxes](/guides/sandboxes) and [Troubleshooting](/guides/troubleshooting). BONNIE refuses a default sandbox that cannot confine tool execution; it does not silently select host execution.
