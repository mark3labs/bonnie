package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// TestReadImageReachesModelAfterReplay runs the sandbox read_file tool through
// a real Kit. A second Runner has a new model, provider, and work directory. Only
// the reopened SQLite journal carries the image into its model request.
// Text-only tool output or replay must fail this test; the source file is
// removed before the second turn, so reading it again cannot hide data loss.
func TestReadImageReachesModelAfterReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const runID = "read-image-replay"

	// Make a valid PNG and put it in the first sandbox's work directory.
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode image: %v", err)
	}
	wantData := base64.StdEncoding.EncodeToString(encoded.Bytes())
	providerA := Local(WithLocalRoot(t.TempDir()))
	path := filepath.Join(providerA.WorkingDir(runID), "chart.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create work directory: %v", err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	// Process A reads the image and finishes its turn.
	dir := t.TempDir()
	journalA, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("open journal A: %v", err)
	}
	t.Cleanup(func() { _ = journalA.Close() })
	modelA := fakemodel.New(
		fakemodel.Call("read_file", `{"path":"chart.png"}`),
		fakemodel.Say("Image read."),
	)
	hermetic := func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}
	runA, err := runtime.NewRunner(journalA, Agent(providerA, hermetic, modelA.Option())).Start(ctx, runID, runtime.Input{Text: "Read chart.png."})
	if err != nil {
		t.Fatalf("start A: %v", err)
	}
	if runA.State != runtime.RunCompleted || runA.Response != "Image read." {
		t.Fatalf("run A = %q %q, want completed with scripted reply", runA.State, runA.Response)
	}
	requestsA := modelA.Requests()
	if len(requestsA) != 2 || modelA.Remaining() != 0 {
		t.Fatalf("process A made %d requests with %d replies left, want 2 and 0", len(requestsA), modelA.Remaining())
	}
	original := assertReadImageMedia(t, requestsA[1].Messages, wantData)
	if err := journalA.Close(); err != nil {
		t.Fatalf("close journal A: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove source image: %v", err)
	}

	// Process B restores the messages and then sends them to a fresh model.
	journalB, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("open journal B: %v", err)
	}
	t.Cleanup(func() { _ = journalB.Close() })
	restored, err := runtime.Restore(ctx, runID, journalB)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := assertReadImageMedia(t, restored.GetMessages(), wantData); got != original {
		t.Fatalf("restored media = %+v, want %+v", got, original)
	}
	modelB := fakemodel.New(fakemodel.Say("Image remembered."))
	providerB := Local(WithLocalRoot(t.TempDir()))
	runB, err := runtime.NewRunner(journalB, Agent(providerB, hermetic, modelB.Option())).Start(ctx, runID, runtime.Input{Text: "Describe the image you read."})
	if err != nil {
		t.Fatalf("start B: %v", err)
	}
	if runB.State != runtime.RunCompleted || runB.Response != "Image remembered." {
		t.Fatalf("run B = %q %q, want completed with scripted reply", runB.State, runB.Response)
	}
	requestsB := modelB.Requests()
	if len(requestsB) != 1 || modelB.Remaining() != 0 {
		t.Fatalf("process B made %d requests with %d replies left, want 1 and 0", len(requestsB), modelB.Remaining())
	}
	if got := assertReadImageMedia(t, requestsB[0].Messages, wantData); got != original {
		t.Fatalf("replayed model media = %+v, want %+v", got, original)
	}
}

// assertReadImageMedia checks the tool pair and the complete image payload,
// not the display text that BONNIE stores beside the message payload.
func assertReadImageMedia(t *testing.T, messages []kit.LLMMessage, wantData string) kit.LLMToolResultOutputContentMedia {
	t.Helper()
	var callIDs, resultIDs []string
	var media []kit.LLMToolResultOutputContentMedia
	for _, msg := range messages {
		for _, part := range msg.Content {
			switch v := part.(type) {
			case kit.LLMToolCallPart:
				if v.ToolName == "read_file" {
					callIDs = append(callIDs, v.ToolCallID)
				}
			case kit.LLMToolResultPart:
				resultIDs = append(resultIDs, v.ToolCallID)
				switch output := v.Output.(type) {
				case kit.LLMToolResultOutputContentMedia:
					media = append(media, output)
				case *kit.LLMToolResultOutputContentMedia:
					media = append(media, *output)
				default:
					t.Fatalf("tool result output = %T, want image media", v.Output)
				}
			}
		}
	}
	if len(callIDs) != 1 || len(resultIDs) != 1 || callIDs[0] == "" || callIDs[0] != resultIDs[0] {
		t.Fatalf("read_file call IDs = %v, result IDs = %v; want one matching pair", callIDs, resultIDs)
	}
	if len(media) != 1 {
		t.Fatalf("media results = %d, want 1", len(media))
	}
	if media[0].Data != wantData || media[0].MediaType != "image/png" {
		t.Fatalf("media = %+v, want original PNG bytes and image/png", media[0])
	}
	return media[0]
}
