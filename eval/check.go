package eval

import (
	"fmt"
	"regexp"
	"strings"
)

// Matcher is a deterministic check of one string. Match returns whether s
// matches, and when it does not, why.
type Matcher interface {
	Match(s string) (ok bool, why string)
}

// MatcherFunc adapts a function to [Matcher].
type MatcherFunc func(s string) (ok bool, why string)

// Match calls f.
func (f MatcherFunc) Match(s string) (bool, string) { return f(s) }

// Check is a gate: it fails the eval when m does not match value. It
// returns whether value matched.
func (e *T) Check(value string, m Matcher) bool {
	e.tb.Helper()
	ok, why := m.Match(value)
	if !ok {
		e.tb.Errorf("bonnie: eval: check failed: %s", why)
	}
	return ok
}

// Contains matches a string that contains sub.
func Contains(sub string) Matcher {
	return MatcherFunc(func(s string) (bool, string) {
		if strings.Contains(s, sub) {
			return true, ""
		}
		return false, fmt.Sprintf("%s does not contain %q", short(s), sub)
	})
}

// ContainsFold matches a string that contains sub, with case ignored.
func ContainsFold(sub string) Matcher {
	return MatcherFunc(func(s string) (bool, string) {
		if strings.Contains(strings.ToLower(s), strings.ToLower(sub)) {
			return true, ""
		}
		return false, fmt.Sprintf("%s does not contain %q (case ignored)", short(s), sub)
	})
}

// Equals matches a string equal to want after leading and trailing white
// space is removed from both.
func Equals(want string) Matcher {
	return MatcherFunc(func(s string) (bool, string) {
		if strings.TrimSpace(s) == strings.TrimSpace(want) {
			return true, ""
		}
		return false, fmt.Sprintf("%s is not %q", short(s), want)
	})
}

// Matches matches a string that the regular expression pattern matches.
// An invalid pattern matches nothing.
func Matches(pattern string) Matcher {
	re, err := regexp.Compile(pattern)
	return MatcherFunc(func(s string) (bool, string) {
		if err != nil {
			return false, fmt.Sprintf("invalid pattern %q: %v", pattern, err)
		}
		if re.MatchString(s) {
			return true, ""
		}
		return false, fmt.Sprintf("%s does not match %q", short(s), pattern)
	})
}

// Not matches a string that m does not match.
func Not(m Matcher) Matcher {
	return MatcherFunc(func(s string) (bool, string) {
		if ok, _ := m.Match(s); ok {
			return false, fmt.Sprintf("%s matches, and must not", short(s))
		}
		return true, ""
	})
}

// short quotes s for a failure message, cut to a readable length.
func short(s string) string {
	const limit = 200
	if r := []rune(s); len(r) > limit {
		return fmt.Sprintf("%q...", string(r[:limit]))
	}
	return fmt.Sprintf("%q", s)
}
