// Command serves the self-contained-agent agent tree: the customer FAQ
// assistant of a computer repair shop. Its model runs in this process, on the
// CPU, with no provider key and no model server. After the first start, it
// makes no network call for the model. It answers from context/faq.md, which
// BONNIE copies into each run.
//
// This tree was made with `bonnie init` and is run with the bonnie CLI, the
// same way as your own agent:
//
//	bonnie dev                           serve with hot reload and a terminal
//	bonnie build                         one static binary
//	bonnie runs list --journal .bonnie   inspect the durable runs
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, context/ is the
// agent's root for files — and bonnie_gen.go is the wiring `bonnie dev` and
// `bonnie build` regenerate. Everything else is an option below.
//
// The model is LiquidAI's LFM2.5-2.6B, from Hugging Face, run by Kronk
// (github.com/ardanlabs/kronk) on llama.cpp, through fantasy's Kronk
// provider. kronk.go registers it with Kit: Kit gets a model for
// "kronk/<source>" from it. The first turn downloads llama.cpp and the model
// (about 1.7 GB) under ~/.kronk, then loads the model. Later turns and later
// starts use the files on disk.
package main

import (
	"os"

	fkronk "charm.land/fantasy/providers/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// modelSource is the model Kronk downloads: LFM2.5-2.6B from LiquidAI's GGUF
// repository on Hugging Face, quantized to Q4_K_M (about 1.7 GB). LiquidAI
// trained it for tool use; its tool calls go through Kronk's LFM parser.
//
// It is a reasoning model, and its chat template has no switch to turn the
// reasoning off. Kronk keeps the reasoning out of the answer. A template that
// skips the reasoning does not make a turn faster: the model then writes the
// reasoning into the answer.
const modelSource = "LiquidAI/LFM2.5-2.6B-GGUF:Q4_K_M"

// contextWindow is the context window in tokens. The model takes up to 128k;
// 32k holds the prompt, faq.md and a long conversation, and uses less memory.
const contextWindow = 32_768

// samplingDefaults are the settings the model card recommends. The low
// temperature keeps the answers close to the text of faq.md. A value in the
// call replaces each one; Kronk fills the fields that are zero.
var samplingDefaults = model.Params{
	Temperature:   0.1,
	TopK:          50,
	RepeatPenalty: 1.1,
}

func main() {
	// Run on the CPU. Kronk picks its llama.cpp build from KRONK_PROCESSOR,
	// or else from the GPUs it detects. The CPU build needs no GPU driver
	// and no GPU library, so the agent runs the same on each machine.
	os.Setenv("KRONK_PROCESSOR", "cpu")

	bonnie.New(
		// The name GET /bonnie/v1/info reports.
		bonnie.WithName("self-contained-agent"),

		// "kronk/..." resolves through the provider registered below, not
		// through a built-in provider, so no API key is necessary.
		bonnie.WithModel(providerName+"/"+modelSource),
		bonnie.WithKit(kit.WithProvider(providerName, kronkProvider(
			fkronk.WithDefaultParams(samplingDefaults),
			fkronk.WithContextWindow(contextWindow),
		))),

		// A tool call, for example read_file on faq.md, runs in a sandbox
		// process that starts from a clean environment. That process is this
		// binary, which loads libffi when it starts (see README.md). When the
		// host finds libffi through LD_LIBRARY_PATH, as a Nix shell does, the
		// sandbox needs the same value.
		bonnie.WithSandboxEnv(map[string]string{
			"LD_LIBRARY_PATH": os.Getenv("LD_LIBRARY_PATH"),
		}),
	).Serve()
}
