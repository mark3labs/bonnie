// Package workspaceskills discovers untrusted, on-demand workspace skills.
package workspaceskills

import (
	"fmt"
	"path"
	"strings"
)

// Policy bounds workspace discovery. Zero fields select defaults.
type Policy struct {
	Exclude          []string
	MaxDepth         int
	MaxSkills        int
	MaxSkillBytes    int64
	MaxMetadataBytes int64
}

// Normalize validates a copied policy and fills its defaults.
func Normalize(p Policy) (Policy, error) {
	if p.MaxDepth < 0 || p.MaxSkills < 0 || p.MaxSkillBytes < 0 || p.MaxMetadataBytes < 0 {
		return p, fmt.Errorf("negative workspace skill limit")
	}
	if p.MaxSkillBytes > 16<<20 || p.MaxSkills > 10000 || p.MaxMetadataBytes > 16<<20 || p.MaxDepth > 256 {
		return p, fmt.Errorf("workspace skill limits exceed supported ceilings")
	}
	if p.MaxDepth == 0 {
		p.MaxDepth = 12
	}
	if p.MaxSkills == 0 {
		p.MaxSkills = 100
	}
	if p.MaxSkillBytes == 0 {
		p.MaxSkillBytes = 128 << 10
	}
	if p.MaxMetadataBytes == 0 {
		p.MaxMetadataBytes = 32 << 10
	}
	p.Exclude = append([]string(nil), p.Exclude...)
	for _, g := range p.Exclude {
		if strings.HasPrefix(g, "!") || strings.HasPrefix(g, "/") || strings.ContainsRune(g, 0) {
			return p, fmt.Errorf("invalid workspace exclusion %q", g)
		}
		for s := range strings.SplitSeq(g, "/") {
			if s == "" || s == "." || s == ".." {
				return p, fmt.Errorf("invalid workspace exclusion %q", g)
			}
			if s == "**" {
				continue
			}
			if strings.Contains(s, "**") {
				return p, fmt.Errorf("** must be a whole exclusion segment")
			}
			if _, err := path.Match(s, ""); err != nil {
				return p, fmt.Errorf("invalid workspace exclusion %q", g)
			}
		}
	}
	return p, nil
}
func excluded(pattern, p string) bool {
	var match func([]string, []string) bool
	match = func(gs, ps []string) bool {
		if len(gs) == 0 {
			return len(ps) == 0
		}
		if gs[0] == "**" {
			return match(gs[1:], ps) || (len(ps) > 0 && match(gs, ps[1:]))
		}
		if len(ps) == 0 {
			return false
		}
		ok, _ := path.Match(gs[0], ps[0])
		return ok && match(gs[1:], ps[1:])
	}
	return match(strings.Split(pattern, "/"), strings.Split(p, "/"))
}
