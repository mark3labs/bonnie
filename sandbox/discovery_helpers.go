package sandbox

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
)

// Release builds generate both Linux helpers before compiling BONNIE.
// Source builds without these assets fail enabled remote discovery explicitly.
//
//go:embed discoveryhelpers/*
var discoveryHelpers embed.FS

func discoveryHelper(arch string) ([]byte, error) {
	compressed, err := discoveryHelpers.ReadFile("discoveryhelpers/linux-" + arch + ".gz")
	if err != nil {
		return nil, fmt.Errorf("bonnie: secure discovery helper for Linux %s is not packaged; run scripts/build-skillfs.sh before building", arch)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 16<<20 {
		return nil, fmt.Errorf("bonnie: discovery helper exceeds size bound")
	}
	return data, nil
}
