# self-contained-agent

The customer FAQ assistant of a computer repair shop, as an agent that needs
nothing outside its own process: no provider key, no model server, and no
GPU. The model is [Qwen3-1.7B](https://huggingface.co/Qwen/Qwen3-1.7B).
[Kronk](https://github.com/ardanlabs/kronk) runs it on the CPU, in the agent's
own process, with llama.cpp. The shop's facts (opening hours, services and
prices, warranty, payment, contact) are in `context/faq.md`.

It is also a reference for a **custom model provider**. `kronk.go` registers
[fantasy's Kronk provider](https://github.com/charmbracelet/fantasy/tree/main/providers/kronk)
with Kit under the name `kronk`, and `main.go` selects the model
`kronk/unsloth/Qwen3-1.7B-Q4_K_M`. Kit gives the part after `kronk/` to the
provider, and the provider downloads and loads that model.

## This directory is an agent tree

This directory was made with `bonnie init`. It is its own Go module, and you
run and manage it with the `bonnie` CLI, the same way as your own agent:

| File | What it is |
|---|---|
| `instructions.md` | the system prompt: answer from `faq.md` only |
| `context/faq.md` | the shop's FAQ, copied into each run; the shop and its details are fictional |
| `main.go` | the `bonnie.New(...)` call: the model, its settings, the CPU, and the `kronk` provider |
| `kronk.go` | the Kit provider factory around fantasy's Kronk provider, and its log file |
| `bonnie_gen.go` | the wiring `bonnie dev` and `bonnie build` regenerate; do not edit |
| `skills/` | the agent's skills, empty here |
| `go.mod`, `go.sum` | pin a released BONNIE, and the Kronk release that fantasy builds against |

## What you need

- The CLI: `go install github.com/mark3labs/bonnie/cmd/bonnie@latest`.
- libffi. Kronk calls llama.cpp through it, and the binary loads it when it
  starts: `libffi.so.8` on Linux, `libffi.8.dylib` on macOS. Without it, each
  command stops with `libffi.so.8: cannot open shared object file`. Most Linux
  systems have it. If yours does not, install it (`apt install libffi8`,
  `dnf install libffi`, or `brew install libffi`). On NixOS, use the
  repository's dev shell (`nix develop`), which also supplies the `libstdc++`
  that llama.cpp needs.
- About 1.2 GB of disk space under `~/.kronk` and a network connection for
  the first start.

## Run it

```bash
cd examples/self-contained-agent
bonnie dev
```

In the terminal, ask:

```text
Read faq.md and tell me the Saturday opening hours.
```

**The first turn is slow.** The provider loads the model the first time Kit
asks for it. On the first start, it also downloads llama.cpp and the 1 GB
model into `~/.kronk`. Kronk's log, with the download progress, is in
`.bonnie/kronk.log` (`tail -f .bonnie/kronk.log`), not in the terminal, where
it would print over the `bonnie dev` interface. Later turns use the loaded
model. On a laptop CPU, an answer that reads `faq.md` takes about 10–20
seconds, because the model runs once to call the tool and once to answer.
Later starts load the model from disk and download nothing.

Or serve it and speak to it over HTTP:

```bash
bonnie dev --addr 127.0.0.1:8081 --tui=false
curl -s localhost:8081/bonnie/v1/runs -d '{"text":"Read faq.md: do you fix game consoles?"}'
```

## What to look at

**A provider is a factory.** `kronkProvider` in `kronk.go` is a
`kit.ProviderFactory`. `main.go` registers it for this agent with
`bonnie.WithKit(kit.WithProvider("kronk", ...))`, and selects it with
`bonnie.WithModel("kronk/...")`. BONNIE builds a new Kit for each turn of a
run, and Kit calls the factory each time. Thus the factory shares one fantasy
provider, which keeps each model loaded, and it does not set a `Closer`,
which would unload the model at the end of each turn.

**fantasy does the conversion.** fantasy's Kronk provider downloads the
model, and converts Kit's prompt, tools and settings to Kronk's chat request.
It converts the answer back: text, tool calls and token usage.

**The CPU, always.** `main.go` sets `KRONK_PROCESSOR=cpu`, so Kronk installs
and loads its CPU build of llama.cpp. That build needs no GPU driver and no
GPU library, so the agent runs the same on each machine. To use a GPU, remove
that line: Kronk then detects CUDA, ROCm, Vulkan or Metal, and the host must
supply the libraries for that backend.

**No reasoning.** Qwen3 reasons before it answers by default. On the CPU, the
reasoning was most of each turn, so `main.go` turns it off
(`enable_thinking: false`), which halves the time of a turn.

**No FAQ data is in the prompt.** At the start of a turn, the model gets
`instructions.md`, a note about the channel, and the question. The facts
reach it only as the result of a `read_file` call on `faq.md`.
`instructions.md` therefore says, first and last, to call `read_file` with
`{"path": "faq.md"}` before each answer. Its examples use placeholders, not
facts from the FAQ. Write the tool call out like that: when the prompt
described the call in words only, the model called the tool much less often.

**A small model does not always use its tools.** In a test of five plain
questions, asked twice, the model read `faq.md` in 7 of 10 turns, and each
of those answers was correct. In the other turns, it answered without the
tool and invented the facts, or it wrote the tool call as plain JSON text,
which is not run. When the question itself says to read `faq.md`, it does.
For a reliable FAQ agent, put the FAQ text in `instructions.md`, or use a
larger model.

**The sandbox gets the library path.** A tool call runs in a sandbox process
that starts from a clean environment, and that process is this binary, which
loads libffi. `main.go` passes `LD_LIBRARY_PATH` to the sandbox with
`bonnie.WithSandboxEnv`, for a host (such as a Nix shell) that finds libffi
through it.

**The run is durable, the model is local.** Nothing about durability
changes. Each step goes into the journal, and a run that a crash stopped
continues at the next start. See the runs with:

```bash
bonnie runs --journal .bonnie
```

## Use a different model

Change `modelSource` in `main.go`. Kronk takes:

- a model ID from the Kronk catalog: `unsloth/Qwen3-0.6B-Q8_0`
- a Hugging Face GGUF repository and a quantization:
  `LiquidAI/LFM2.5-1.2B-Instruct-GGUF:Q4_K_M`
- the URL of a `.gguf` file on Hugging Face

Set `samplingDefaults`, `contextWindow` and `chatTemplate` to the values in
that model's card. The model must be one that the llama.cpp in the pinned
Kronk release can load, and its tool calls must be in a format that Kronk
parses.
