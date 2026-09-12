// Command bonnie is the BONNIE developer CLI.
//
// The framework is usable as a library without this binary; the CLI exists for
// local development and for inspecting durable runs.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// version is set by the linker at release time.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("bonnie", buildVersion())
	case "help", "--help", "-h":
		usage()
	case "serve":
		err = runServe(os.Args[2:])
	case "runs":
		err = runRuns(os.Args[2:])
	case "sandbox":
		err = runSandbox(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "bonnie: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, prefixed(err))
		os.Exit(1)
	}
}

// prefixed renders an error for the terminal with exactly one "bonnie:" on
// the front.
//
// Library errors are already wrapped `bonnie: context: ...` by convention, so
// prefixing unconditionally produced "bonnie: bonnie: ...". Errors raised by
// the CLI itself carry no prefix and need one.
func prefixed(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "bonnie:") {
		return msg
	}
	return "bonnie: " + msg
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return version
}

func usage() {
	fmt.Fprint(os.Stderr, `BONNIE — Builder Of Neural Network Intelligence Engines

Usage:
  bonnie <command> [flags]

Commands:
  serve      Mount the HTTP channel and serve durable runs
  runs       List and inspect durable runs
  sandbox    Reclaim the sandboxes of finished runs
  version    Print the BONNIE version
  help       Show this message

Run "bonnie <command> -h" for the flags of a command.

serve:
  bonnie serve [--addr :8080] [--journal .bonnie] [--model PROVIDER/MODEL]
                [--sandbox none|docker|microsandbox|local|auto]
                [--sandbox-image IMAGE] [--sandbox-deny-network]

    POST /runs                 start a run, or resolve an address to one
    GET  /runs/{id}            report a run's durable state
    POST /runs/{id}            send a message to an existing run
    POST /runs/{id}/respond    answer a suspended run
    POST /runs/{id}/cancel     stop the turn a run is executing
    GET  /runs/{id}/stream     NDJSON event stream, resumable with ?cursor=

    Without --sandbox, tool calls run as this process, with its files,
    network, and credentials. Use --sandbox docker for a server that is
    reachable from outside. See docs/SANDBOX.md.

runs:
  bonnie runs list [--journal .bonnie] [--state waiting] [--json]
  bonnie runs show <run-id> [--journal .bonnie] [--json]

Planned:
  init       Scaffold an agent/ tree
  dev        Run the agent locally with hot reload
  eval       Run evals against a local or remote agent
`)
}
