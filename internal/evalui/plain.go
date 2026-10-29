package evalui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// labelWidth is the label column of the plain table. A longer label is cut,
// and shown in full on the next line.
const labelWidth = 28

// printer writes to w and keeps the first error, so a long report needs
// one check.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

// WritePlain writes the snapshot as plain text: one block for each eval,
// one row for each score, the failure output of each eval that did not
// pass, and the totals. It has no color, so it fits a log or a pipe.
func WritePlain(w io.Writer, s Snapshot) error {
	p := &printer{w: w}
	for _, line := range s.Build {
		p.printf("%s\n", line)
	}
	for _, r := range s.Evals {
		from := ""
		if r.Carried() {
			from = "  (from " + r.Run + ")"
		}
		p.printf("%s  %s  %.1fs%s\n", statusWord(r.Status), r.Name, r.Elapsed, from)
		for _, rep := range r.Reports {
			label := truncate(rep.Label, labelWidth)
			p.printf("      %-8s %-*s %.3f  %-12s %s\n", rep.Kind, labelWidth, label, rep.Value, Bounds(rep), rep.Status)
			if label != rep.Label {
				p.printf("        label: %s\n", rep.Label)
			}
			if rep.Error != "" {
				p.printf("        error: %s\n", rep.Error)
			} else if rep.Reason != "" && rep.Status != "passed" && rep.Status != "recorded" {
				p.printf("        reason: %s\n", rep.Reason)
			}
		}
		if r.Status != StatusPass {
			for _, line := range r.Failure() {
				p.printf("      %s\n", line)
			}
		}
	}
	p.printf("\n%s\n", Totals(s.Counts()))
	if s.Dir != "" {
		p.printf("saved: %s\n", s.Dir)
	}
	return p.err
}

// WriteJSON writes each score of the snapshot as one JSON line.
func WriteJSON(w io.Writer, s Snapshot) error {
	enc := json.NewEncoder(w)
	for _, r := range s.Evals {
		for _, rep := range r.Reports {
			if err := enc.Encode(rep); err != nil {
				return err
			}
		}
	}
	return nil
}

// Totals renders the totals line.
func Totals(c Counts) string {
	s := fmt.Sprintf("evals: %d passed, %d failed, %d skipped", c.Passed, c.Failed, c.Skipped)
	if c.Running > 0 {
		s += fmt.Sprintf(", %d running", c.Running)
	}
	if n := c.Scores["passed"] + c.Scores["scored"] + c.Scores["failed"] + c.Scores["recorded"]; n > 0 {
		s += fmt.Sprintf("; scores: %d passed, %d scored, %d failed, %d recorded",
			c.Scores["passed"], c.Scores["scored"], c.Scores["failed"], c.Scores["recorded"])
	}
	return s
}

// Bounds renders the threshold of a report, for example ">= 0.800".
func Bounds(rep Report) string {
	switch {
	case rep.Min != nil && rep.Max != nil:
		return fmt.Sprintf("%.2f..%.2f", *rep.Min, *rep.Max)
	case rep.Min != nil:
		return fmt.Sprintf(">= %.3f", *rep.Min)
	case rep.Max != nil:
		return fmt.Sprintf("<= %.3f", *rep.Max)
	}
	return "-"
}

func statusWord(status string) string {
	switch status {
	case StatusPass:
		return "PASS"
	case StatusSkip:
		return "SKIP"
	case StatusRunning:
		return "RUN "
	}
	return "FAIL"
}

// truncate cuts s to n runes, with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
