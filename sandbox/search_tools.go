package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	kit "github.com/mark3labs/kit/pkg/kit"
)

const (
	searchVisitLimit = 10000
	searchFileLimit  = 1 << 20
	searchDuration   = 30 * time.Second
)

type fileGlob []string

func compileGlob(pattern string) (fileGlob, error) {
	if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("pattern must be a nonempty relative glob")
	}
	segments := strings.Split(pattern, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("invalid glob segment")
		}
		if _, err := path.Match(s, ""); err != nil {
			return nil, err
		}
	}
	return segments, nil
}

func (g fileGlob) matches(name string) bool {
	if len(g) == 0 {
		return true
	}
	parts := strings.Split(name, "/")
	type state struct{ i, j int }
	memo := map[state]bool{}
	seen := map[state]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := state{i, j}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		result := false
		switch {
		case i == len(g):
			result = j == len(parts)
		case g[i] == "**":
			result = match(i+1, j) || (j < len(parts) && match(i, j+1))
		case j < len(parts):
			ok, _ := path.Match(g[i], parts[j])
			result = ok && match(i+1, j+1)
		}
		memo[key] = result
		return result
	}
	return match(0, 0)
}

type searchResponse struct {
	Results   []any  `json:"results"`
	Truncated bool   `json:"truncated"`
	Reason    string `json:"reason,omitempty"`
}

func newSearchResponse() *searchResponse { return &searchResponse{Results: []any{}} }
func (r *searchResponse) partial(reason string) {
	r.Truncated = true
	if !strings.Contains(r.Reason, reason) {
		if r.Reason != "" {
			r.Reason += "; "
		}
		r.Reason += reason
	}
}
func (r *searchResponse) add(value any, limit int) bool {
	if len(r.Results) >= limit {
		r.partial("result limit")
		return false
	}
	r.Results = append(r.Results, value)
	data, _ := json.Marshal(r)
	// Reserve room for all partial reasons added after this record.
	if len(data) > maxToolOutput-256 {
		r.Results = r.Results[:len(r.Results)-1]
		r.partial("response byte limit")
		return false
	}
	return true
}
func (r *searchResponse) output() kit.ToolOutput {
	data, err := json.Marshal(r)
	if err != nil {
		return searchError(err)
	}
	return kit.TextResult(string(data))
}
func searchError(err error) kit.ToolOutput {
	text := strings.ToValidUTF8(err.Error(), "�")
	if len(text) > maxToolOutput {
		text = text[:maxToolOutput]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return kit.ErrorResult(text)
}

// walkSearch visits a bounded tree in lexical depth-first order. No descendant
// link is followed. Directory enumeration is not a filesystem snapshot.
func walkSearch(ctx context.Context, sb Sandbox, root string, fileRoot bool, response *searchResponse, visit func(string, string) (bool, error)) error {
	visited := 0
	var walk func(string, string, []DirectoryEntry, bool) (bool, error)
	walk = func(dir, relative string, entries []DirectoryEntry, partial bool) (bool, error) {
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			visited++
			if visited > searchVisitLimit {
				response.partial("entry visit limit")
				return false, nil
			}
			rel := path.Join(relative, entry.Name)
			target := path.Join(dir, entry.Name)
			switch entry.Kind {
			case "directory":
				if entry.Name == ".git" {
					continue
				}
				if visited >= searchVisitLimit {
					response.partial("entry visit limit")
					return false, nil
				}
				children, cut, err := readDirectory(ctx, sb, target, searchVisitLimit-visited)
				if err != nil {
					return false, err
				}
				more, err := walk(target, rel, children, cut)
				if err != nil || !more {
					return more, err
				}
			case "file":
				more, err := visit(target, rel)
				if err != nil || !more {
					return more, err
				}
			}
		}
		if partial {
			response.partial("entry visit limit")
			return false, nil
		}
		return true, nil
	}
	entries, partial, err := readDirectory(ctx, sb, root, searchVisitLimit)
	if errors.Is(err, ErrNotDirectory) && fileRoot {
		_, err = visit(root, path.Base(Resolve(root)))
		return err
	}
	if err != nil {
		return err
	}
	_, err = walk(Resolve(root), "", entries, partial)
	return err
}

func searchLimit(limit int) (int, error) {
	if limit == 0 {
		return 100, nil
	}
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("limit must be between 1 and 1000")
	}
	return limit, nil
}

func findTool(open Opener) kit.Tool {
	type input struct {
		Pattern string `json:"pattern" description:"Required relative glob. ** matches zero or more directories."`
		Path    string `json:"path,omitempty" description:"Directory, default /workspace."`
		Limit   int    `json:"limit,omitempty" description:"Result limit, default 100, maximum 1000."`
	}
	return kit.NewTool("find", "Find regular files in a sandbox. Includes dotfiles, excludes .git directories, does not follow descendant symlinks or honor .gitignore. Returns bounded JSON results and partial status.", func(ctx context.Context, in input) (kit.ToolOutput, error) {
		glob, err := compileGlob(in.Pattern)
		if err != nil {
			return searchError(err), nil
		}
		limit, err := searchLimit(in.Limit)
		if err != nil {
			return searchError(err), nil
		}
		ctx, cancel := context.WithTimeout(ctx, searchDuration)
		defer cancel()
		sb, err := open(ctx)
		if err != nil {
			return searchError(err), nil
		}
		response := newSearchResponse()
		err = walkSearch(ctx, sb, in.Path, false, response, func(_, rel string) (bool, error) {
			if !glob.matches(rel) {
				return true, nil
			}
			return response.add(struct {
				Path string `json:"path"`
			}{rel}, limit), nil
		})
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return searchError(err), nil
		}
		return response.output(), nil
	})
}

type grepLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}
type grepMatch struct {
	Path   string     `json:"path"`
	Line   int        `json:"line"`
	Text   string     `json:"text"`
	Before []grepLine `json:"before"`
	After  []grepLine `json:"after"`
}

func grepTool(open Opener) kit.Tool {
	type input struct {
		Pattern string `json:"pattern" description:"Nonempty Go regexp, or literal substring when literal is true."`
		Path    string `json:"path,omitempty" description:"File or directory, default /workspace."`
		Glob    string `json:"glob,omitempty" description:"Optional relative file glob."`
		Literal bool   `json:"literal,omitempty" description:"Match an exact case-sensitive substring."`
		Context int    `json:"context,omitempty" description:"Context lines before and after, 0 to 10."`
		Limit   int    `json:"limit,omitempty" description:"Matching line limit, default 100, maximum 1000."`
	}
	return kit.NewTool("grep", "Search sandbox text files per line. Returns JSON path, one-based line, text, before/after context, and partial status. Skips NUL files; files above 1 MiB make results partial. No .gitignore filtering. Maximum 10000 entries, 30 seconds, and 32 KiB output.", func(ctx context.Context, in input) (kit.ToolOutput, error) {
		if in.Pattern == "" {
			return searchError(fmt.Errorf("pattern must not be empty")), nil
		}
		if in.Context < 0 || in.Context > 10 {
			return searchError(fmt.Errorf("context must be between 0 and 10")), nil
		}
		limit, err := searchLimit(in.Limit)
		if err != nil {
			return searchError(err), nil
		}
		var glob fileGlob
		if in.Glob != "" {
			glob, err = compileGlob(in.Glob)
			if err != nil {
				return searchError(err), nil
			}
		}
		var matches func(string) bool
		if in.Literal {
			matches = func(line string) bool { return strings.Contains(line, in.Pattern) }
		} else {
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
				return searchError(err), nil
			}
			matches = re.MatchString
		}
		ctx, cancel := context.WithTimeout(ctx, searchDuration)
		defer cancel()
		sb, err := open(ctx)
		if err != nil {
			return searchError(err), nil
		}
		response := newSearchResponse()
		err = walkSearch(ctx, sb, in.Path, true, response, func(target, rel string) (bool, error) {
			if !glob.matches(rel) {
				return true, nil
			}
			data, err := sb.ReadFile(ctx, target)
			if err != nil {
				return false, err
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if len(data) > searchFileLimit {
				response.partial("file exceeds 1 MiB")
				return true, nil
			}
			if bytes.IndexByte(data, 0) >= 0 {
				return true, nil
			}
			lines := strings.Split(string(data), "\n")
			// Only CR immediately before LF is a line terminator.
			for i := 0; i < len(lines)-1; i++ {
				lines[i] = strings.TrimSuffix(lines[i], "\r")
			}
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			for i, line := range lines {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				if !matches(line) {
					continue
				}
				record := grepMatch{Path: rel, Line: i + 1, Text: line, Before: []grepLine{}, After: []grepLine{}}
				for j := max(0, i-in.Context); j < i; j++ {
					record.Before = append(record.Before, grepLine{j + 1, lines[j]})
				}
				for j := i + 1; j < min(len(lines), i+in.Context+1); j++ {
					record.After = append(record.After, grepLine{j + 1, lines[j]})
				}
				if !response.add(record, limit) {
					return false, nil
				}
			}
			return true, nil
		})
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return searchError(err), nil
		}
		return response.output(), nil
	})
}
