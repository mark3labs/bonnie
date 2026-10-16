//go:build linux

// bonnie-skillfs implements only bounded workspace discovery I/O.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/internal/skillfs"
)

func main() {
	response := skillfs.Response{Version: 1}
	var req skillfs.Request
	if len(os.Args) != 2 || len(os.Args[1]) > 8192 {
		os.Exit(2)
	}
	decoder := json.NewDecoder(strings.NewReader(os.Args[1]))
	if err := decoder.Decode(&req); err != nil || req.Version != 1 || len(req.Path) > 4096 {
		response.Error = "invalid discovery protocol"
	} else {
		root, err := skillfs.Open("/workspace")
		if err != nil {
			response.Error = "cannot open workspace"
		} else {
			defer func() { _ = root.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			switch req.Operation {
			case "list":
				response.Entries, response.Partial, err = root.List(ctx, req.Path, int(req.Limit))
			case "read":
				response.File, err = root.Read(ctx, req.Path, req.Limit)
			default:
				response.Error = "unsupported discovery operation"
			}
			if err != nil {
				response.Error = "discovery I/O rejected"
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		os.Exit(1)
	}
}
