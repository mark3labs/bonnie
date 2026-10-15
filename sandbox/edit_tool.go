package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type mutationGate chan struct{}

func (g mutationGate) acquire(ctx context.Context) (func(), error) {
	select {
	case g <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-g
			return nil, err
		}
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type editReplacement struct {
	OldText string  `json:"old_text" description:"Nonempty text which must occur exactly once in the original file."`
	NewText *string `json:"new_text" description:"Replacement text. An empty string deletes the matched text."`
}
type editInput struct {
	Path         string             `json:"path" description:"Required existing sandbox file."`
	OldText      *string            `json:"old_text,omitempty" description:"Single replacement match. Do not combine with replacements."`
	NewText      *string            `json:"new_text,omitempty" description:"Single replacement text; an empty string deletes."`
	Replacements *[]editReplacement `json:"replacements,omitempty" description:"One to 100 exact replacements against the original file."`
}

func (in editInput) validate() ([]editReplacement, error) {
	if strings.TrimSpace(in.Path) == "" {
		return nil, fmt.Errorf("path must not be empty")
	}
	encodedPath, _ := json.Marshal(in.Path)
	if len(encodedPath) > maxToolOutput-256 {
		return nil, fmt.Errorf("path exceeds response byte limit")
	}
	replacements := []editReplacement{}
	if in.Replacements != nil {
		if in.OldText != nil || in.NewText != nil {
			return nil, fmt.Errorf("do not mix single and multiple replacement forms")
		}
		replacements = *in.Replacements
	} else {
		if in.OldText == nil || in.NewText == nil {
			return nil, fmt.Errorf("old_text and new_text are required")
		}
		replacements = append(replacements, editReplacement{*in.OldText, in.NewText})
	}
	if len(replacements) < 1 || len(replacements) > 100 {
		return nil, fmt.Errorf("replacements must contain 1 to 100 objects")
	}
	for i, r := range replacements {
		if r.OldText == "" {
			return nil, fmt.Errorf("replacement %d: old_text must not be empty", i+1)
		}
		if r.NewText == nil {
			return nil, fmt.Errorf("replacement %d: new_text is required", i+1)
		}
		if !utf8.ValidString(r.OldText) || !utf8.ValidString(*r.NewText) || strings.ContainsRune(*r.NewText, 0) || strings.ContainsRune(r.OldText, 0) {
			return nil, fmt.Errorf("replacement %d: text must be UTF-8 without NUL", i+1)
		}
	}
	return replacements, nil
}

// planEdit finds every region against the original bytes, including overlapping
// occurrences. Inserted content is never searched by another replacement.
func planEdit(data []byte, replacements []editReplacement) ([]byte, error) {
	if len(data) > searchFileLimit {
		return nil, fmt.Errorf("file exceeds 1 MiB")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("file must be UTF-8 text without NUL")
	}
	type region struct {
		start, end, index int
		text              string
	}
	regions := make([]region, 0, len(replacements))
	for i, r := range replacements {
		old := []byte(r.OldText)
		start := bytes.Index(data, old)
		if start < 0 {
			return nil, fmt.Errorf("replacement %d: old_text is missing", i+1)
		}
		if bytes.Contains(data[start+1:], old) {
			return nil, fmt.Errorf("replacement %d: old_text is ambiguous", i+1)
		}
		regions = append(regions, region{start, start + len(old), i + 1, *r.NewText})
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].start < regions[j].start })
	for i := 1; i < len(regions); i++ {
		if regions[i].start < regions[i-1].end {
			return nil, fmt.Errorf("replacement %d overlaps replacement %d", regions[i].index, regions[i-1].index)
		}
	}
	result := bytes.Clone(data)
	for _, r := range slices.Backward(regions) {
		next := make([]byte, 0, len(result)-(r.end-r.start)+len(r.text))
		next = append(next, result[:r.start]...)
		next = append(next, r.text...)
		next = append(next, result[r.end:]...)
		result = next
	}
	return result, nil
}

func editTool(open Opener, gate mutationGate) kit.Tool {
	return kit.NewTool("edit", "Apply exact single or multiple replacements to one existing UTF-8 sandbox file (maximum 1 MiB, no NUL). Each old_text must occur exactly once; regions must not overlap. Validation failures write nothing. Serialized with write_file only in this tool set, not shell/external writers. A write failure does not promise rollback.", func(ctx context.Context, in editInput) (kit.ToolOutput, error) {
		replacements, err := in.validate()
		if err != nil {
			return searchError(err), nil
		}
		release, err := gate.acquire(ctx)
		if err != nil {
			return searchError(err), nil
		}
		defer release()
		sb, err := open(ctx)
		if err != nil {
			return searchError(err), nil
		}
		data, err := sb.ReadFile(ctx, in.Path)
		if err != nil {
			return searchError(err), nil
		}
		data, err = planEdit(data, replacements)
		if err != nil {
			return searchError(err), nil
		}
		if err := ctx.Err(); err != nil {
			return searchError(err), nil
		}
		if err := sb.WriteFile(ctx, in.Path, data); err != nil {
			return searchError(err), nil
		}
		record := struct {
			Path         string `json:"path"`
			Replacements int    `json:"replacements"`
			Bytes        int    `json:"bytes"`
		}{in.Path, len(replacements), len(data)}
		output, _ := json.Marshal(record)
		return kit.TextResult(string(output)), nil
	})
}
