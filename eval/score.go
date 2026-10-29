package eval

import (
	"fmt"
)

// Kinds of [Score].
const (
	// KindJudge is a score from the decision judge.
	KindJudge = "judge"
	// KindLLM is a score from the language-model judge.
	KindLLM = "llm"
	// KindDecision is a score from a decision the agent made itself.
	KindDecision = "decision"
	// KindScore is a score the eval computed itself.
	KindScore = "score"
)

// Statuses of a [Report].
const (
	// StatusRecorded is a score with no threshold.
	StatusRecorded = "recorded"
	// StatusPassed is a score that meets its threshold.
	StatusPassed = "passed"
	// StatusScored is a score that misses a soft threshold. It does not
	// fail the eval unless strict mode is on.
	StatusScored = "scored"
	// StatusFailed is a score that misses a gate, misses a soft threshold
	// in strict mode, or has no value because its judge failed.
	StatusFailed = "failed"
)

// ReportPrefix starts the log line that reports one score when the eval
// ends. The rest of the line is a JSON [Report]. `bonnie eval` reads these
// lines from the output of go test.
const ReportPrefix = "bonnie-eval: "

// RunPrefix starts the log line that names the kept journal of an eval
// when it ends. The rest of the line is a JSON [RunInfo]. The line is
// logged only when [EnvDir] is set.
const RunPrefix = "bonnie-eval-run: "

// RunInfo is the record of one eval's run, as the eval logs it when it
// ends.
type RunInfo struct {
	Test string `json:"test"`
	// Journal is the directory of the eval's journal. Open it with
	// runtime.OpenSQLiteJournal; the run ID is [RunID].
	Journal string `json:"journal"`
	Turns   int    `json:"turns"`
}

// Report is the record of one score, as the eval logs it when it ends.
type Report struct {
	Test   string   `json:"test"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"`
	Value  float64  `json:"value"`
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Gate   bool     `json:"gate,omitempty"`
	Status string   `json:"status"`
	Model  string   `json:"model,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// Score is a value from 0 to 1 that an eval records: a judgment, a decision
// of the agent, or a number the eval computed. A score with no threshold is
// only recorded. Its methods set a threshold and return the score, so they
// chain:
//
//	e.Judge(eval.Bool("polite", "Is the reply polite?")).AtLeast(0.8).Gate()
//
// A score whose judge failed has no value. It fails the eval when it is
// made, gate or not.
type Score struct {
	e      *T
	kind   string
	label  string
	value  float64
	min    *float64
	max    *float64
	gate   bool
	model  string
	reason string
	err    error
	// failed is true when the score has failed the test, so a later
	// method does not report it again.
	failed bool
}

// Value returns the score, from 0 to 1. It is 0 when the score has an error.
func (s *Score) Value() float64 { return s.value }

// Reason returns the judge's reason, when it gave one.
func (s *Score) Reason() string { return s.reason }

// Err returns why the score has no value, or nil.
func (s *Score) Err() error { return s.err }

// Label names the score in the report. The default is the question ID, or
// the label given to [T.Score].
func (s *Score) Label(name string) *Score {
	s.label = name
	return s
}

// AtLeast sets a soft threshold: the score must be min or more. A miss
// marks the eval "scored", and fails it in strict mode or after [Score.Gate].
func (s *Score) AtLeast(min float64) *Score {
	s.e.tb.Helper()
	s.min = &min
	s.check()
	return s
}

// AtMost sets a soft threshold: the score must be max or less. A miss
// marks the eval "scored", and fails it in strict mode or after [Score.Gate].
func (s *Score) AtMost(max float64) *Score {
	s.e.tb.Helper()
	s.max = &max
	s.check()
	return s
}

// Gate makes the thresholds of the score a gate: a miss fails the eval. It
// needs [Score.AtLeast] or [Score.AtMost], before or after it.
func (s *Score) Gate() *Score {
	s.e.tb.Helper()
	s.gate = true
	if s.min == nil && s.max == nil && s.err == nil {
		s.e.tb.Errorf("bonnie: eval: score %q: Gate needs AtLeast or AtMost", s.label)
		s.failed = true
		return s
	}
	s.check()
	return s
}

// Passed reports whether the score has a value that meets each threshold.
func (s *Score) Passed() bool {
	return s.err == nil && !s.missed()
}

func (s *Score) missed() bool {
	return (s.min != nil && s.value < *s.min) || (s.max != nil && s.value > *s.max)
}

// check fails the test when a threshold is missed and the miss is fatal.
func (s *Score) check() {
	s.e.tb.Helper()
	if s.failed || s.err != nil || !s.missed() || (!s.gate && !s.e.cfg.strict) {
		return
	}
	s.failed = true
	s.e.tb.Errorf("bonnie: eval: score %q is %.3f, %s%s", s.label, s.value, s.bounds(), s.because())
}

// fail records err and fails the test.
func (s *Score) fail(err error) {
	s.e.tb.Helper()
	s.err = err
	s.failed = true
	s.e.tb.Errorf("bonnie: eval: score %q: %v", s.label, err)
}

func (s *Score) bounds() string {
	switch {
	case s.min != nil && s.max != nil:
		return fmt.Sprintf("want %.3f to %.3f", *s.min, *s.max)
	case s.min != nil:
		return fmt.Sprintf("want at least %.3f", *s.min)
	default:
		return fmt.Sprintf("want at most %.3f", *s.max)
	}
}

func (s *Score) because() string {
	if s.reason == "" {
		return ""
	}
	return ": " + s.reason
}

func (s *Score) report(test string, strict bool) Report {
	r := Report{Test: test, Label: s.label, Kind: s.kind, Value: s.value, Min: s.min, Max: s.max, Gate: s.gate, Model: s.model, Reason: s.reason}
	switch {
	case s.err != nil:
		r.Status, r.Error = StatusFailed, s.err.Error()
	case s.min == nil && s.max == nil:
		r.Status = StatusRecorded
	case !s.missed():
		r.Status = StatusPassed
	case s.gate || strict:
		r.Status = StatusFailed
	default:
		r.Status = StatusScored
	}
	return r
}
