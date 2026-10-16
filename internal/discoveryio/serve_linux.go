//go:build linux

package discoveryio

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"time"
)

// Serve performs one fixed helper operation and writes a bounded response.
func Serve(args []string, out io.Writer) error {
	response := Response{Version: ProtocolVersion}
	if len(args) != 5 || args[0] != "1" {
		response.Error = "protocol"
		return json.NewEncoder(out).Encode(response)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, err := Open(args[1])
	if err != nil {
		response.Error = "root"
		return json.NewEncoder(out).Encode(response)
	}
	defer func() { _ = root.Close() }()
	limit, err := strconv.ParseInt(args[4], 10, 64)
	if err != nil {
		response.Error = "limit"
	} else {
		switch args[2] {
		case "list":
			response.Entries, response.More, err = root.List(ctx, args[3], int(limit))
		case "read":
			var file File
			file, err = root.Read(ctx, args[3], limit)
			if err == nil {
				response.File = &file
			}
		default:
			response.Error = "operation"
		}
	}
	if err != nil {
		response.Error = "io"
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(raw) > MaxProtocolBytes {
		return io.ErrShortBuffer
	}
	_, err = out.Write(raw)
	return err
}
