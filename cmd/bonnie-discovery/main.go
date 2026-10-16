//go:build linux

// bonnie-discovery is the static, fixed-operation guest filesystem helper.
package main

import (
	"github.com/mark3labs/bonnie/internal/discoveryio"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--bonnie-discovery-helper-v1" {
		args = args[1:]
	}
	if err := discoveryio.Serve(args, os.Stdout); err != nil {
		os.Exit(2)
	}
}
