package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Images use Kit's image response, while text keeps its existing output.
// Renamed and damaged images must not fall through to binary text.
func TestReadFileImages(t *testing.T) {
	t.Parallel()
	var pngData, jpegData, gifData, largeData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 12, 8))
	for _, err := range []error{
		png.Encode(&pngData, img),
		jpeg.Encode(&jpegData, img, nil),
		gif.Encode(&gifData, img, nil),
		png.Encode(&largeData, image.NewRGBA(image.Rect(0, 0, 2000, 10))),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, path, mime string
		data             []byte
		wantError        bool
	}{
		{"png", "picture.png", "image/png", pngData.Bytes(), false},
		{"jpeg", "picture.jpeg", "image/jpeg", jpegData.Bytes(), false},
		{"gif", "picture.gif", "image/gif", gifData.Bytes(), false},
		{"renamed", "picture.txt", "image/png", pngData.Bytes(), false},
		{"uppercase", "picture.PNG", "image/png", pngData.Bytes(), false},
		{"damaged", "picture.png", "", []byte("not a png"), true},
		{"damaged webp", "picture.webp", "", []byte("not a webp"), true},
		{"text", "note.txt", "", []byte("unchanged text"), false},
		{"resize", "large.png", "image/png", largeData.Bytes(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := openSandbox(t, Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()), "image-read")
			ctx := context.Background()
			if err := sb.WriteFile(ctx, tc.path, tc.data); err != nil {
				t.Fatal(err)
			}
			tool := readFileTool(func(context.Context) (Sandbox, error) { return sb, nil })
			input, err := json.Marshal(map[string]string{"path": tc.path})
			if err != nil {
				t.Fatal(err)
			}
			res, err := tool.Run(ctx, kit.LLMToolCall{Input: string(input)})
			if err != nil || res.IsError != tc.wantError {
				t.Fatalf("read_file: %v, %+v", err, res)
			}
			if strings.Contains(res.Content, "bonnie-read-image-") {
				t.Fatalf("host path in summary: %s", res.Content)
			}
			if tc.mime != "" {
				if res.Type != "image" || res.MediaType != tc.mime || len(res.Data) == 0 {
					t.Fatalf("image result: type=%s mime=%s bytes=%d", res.Type, res.MediaType, len(res.Data))
				}
				if tc.name == "resize" && !strings.Contains(res.Content, "resized from") {
					t.Fatalf("missing resize summary: %s", res.Content)
				}
			} else if !tc.wantError && res.Content != string(tc.data) {
				t.Fatalf("text changed: %q", res.Content)
			}
		})
	}
}

// Kit rejects an image above its ingestion limit without decoding it.
func TestReadSandboxImageAboveIngestionLimit(t *testing.T) {
	t.Parallel()
	data := make([]byte, 21*1024*1024)
	copy(data, []byte("\x89PNG\r\n\x1a\n"))
	res, err := readSandboxImage(context.Background(), data, "too-large.png")
	if err != nil || !res.IsError || len(res.Data) != 0 || !strings.Contains(res.Content, "image limit") {
		t.Fatalf("oversized image: %v, %+v", err, res)
	}
}

// A sandbox path must never cause the image reader to read a host file.
func TestReadFileImageCannotEscapeSandbox(t *testing.T) {
	t.Parallel()
	sb := openSandbox(t, Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()), "image-escape")
	tool := readFileTool(func(context.Context) (Sandbox, error) { return sb, nil })
	res, err := tool.Run(context.Background(), kit.LLMToolCall{Input: `{"path":"../../outside.png"}`})
	if err != nil || !res.IsError || len(res.Data) != 0 {
		t.Fatalf("escape result: %v, %+v", err, res)
	}
}
