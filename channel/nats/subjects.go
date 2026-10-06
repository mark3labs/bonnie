package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	gonats "github.com/nats-io/nats.go"
)

// Subjects contains the literal protocol subjects. Answers, Commands, and
// Queries are bases whose worker routes append a single worker token.
type Subjects struct {
	Tasks    string
	Results  string
	Answers  string
	Events   string
	Commands string
	Queries  string
}

// ResolveSubjects fills empty subjects from root and validates the final routes.
// Explicit subjects win. A root enables the complete status/control protocol.
func ResolveSubjects(root string, s Subjects) (Subjects, error) {
	if root != "" && !validSubject(root) {
		return s, errors.New("bonnie: nats: root must be a literal subject")
	}
	if root != "" {
		for _, x := range []struct {
			p      *string
			suffix string
		}{{&s.Tasks, "tasks"}, {&s.Results, "results"}, {&s.Answers, "answers"}, {&s.Events, "events"}, {&s.Commands, "commands"}, {&s.Queries, "queries"}} {
			if *x.p == "" {
				*x.p = root + "." + x.suffix
			}
		}
	}
	all := []string{s.Tasks, s.Results, s.Answers, s.Events, s.Commands, s.Queries}
	for i, a := range all {
		if a == "" && i >= 3 {
			continue
		}
		if !validSubject(a) {
			return s, errors.New("bonnie: nats: subjects must be literal")
		}
		for j, b := range all[:i] {
			if b == "" {
				continue
			}
			if a == b || (j >= 2 && j != 3 && strings.HasPrefix(a, b+".")) || (i >= 2 && i != 3 && strings.HasPrefix(b, a+".")) {
				return s, errors.New("bonnie: nats: subjects overlap")
			}
		}
	}
	return s, nil
}

// DefaultInputStreamName returns the stable input stream name for a task subject.
func DefaultInputStreamName(subject string) string { return streamName("bonnie-input-", subject) }

// DefaultResultStreamName returns the stable output stream name for a result subject.
func DefaultResultStreamName(subject string) string { return streamName("bonnie-results-", subject) }

// DefaultEventStreamName returns the stable status stream name for an event subject.
func DefaultEventStreamName(subject string) string { return streamName("bonnie-events-", subject) }

func streamName(prefix, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return prefix + hex.EncodeToString(sum[:])
}

// EnsureStream creates a missing file-backed stream only when create is true.
// Existing resources must contain all subjects with limits retention; they are
// never changed. Operators can provision different limits and replication.
func EnsureStream(ctx context.Context, js gonats.JetStreamContext, name string, subjects []string, create bool) error {
	info, err := js.StreamInfo(name, gonats.Context(ctx))
	if errors.Is(err, gonats.ErrStreamNotFound) && create {
		info, err = js.AddStream(&gonats.StreamConfig{Name: name, Subjects: subjects, Storage: gonats.FileStorage}, gonats.Context(ctx))
		if err != nil {
			info, err = js.StreamInfo(name, gonats.Context(ctx))
		}
	}
	if err != nil {
		return fmt.Errorf("bonnie: nats: stream: %w", err)
	}
	if info.Config.Retention != gonats.LimitsPolicy {
		return errors.New("bonnie: nats: stream requires limits retention")
	}
	for _, s := range subjects {
		if !slices.Contains(info.Config.Subjects, s) {
			return errors.New("bonnie: nats: stream does not cover literal protocol subjects")
		}
	}
	return nil
}
