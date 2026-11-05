package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Workflow is a typed definition returned by Define. Register all versions
// that have unfinished runs with the service.
type Workflow interface{ workflowDefinition() definition }

type definition struct {
	name, version, inputType, resultType string
	invoke                               func(context.Context, json.RawMessage) (json.RawMessage, error)
}

// Definition describes one immutable version of a workflow function. The
// function must use only workflow operations for effects and durable waits.
// It must not start goroutines or recover the internal parking panic.
type Definition[I, O any] struct{ def definition }

// Define names a workflow and its version. Names and versions must not be
// empty. NewService checks definitions before it opens execution.
func Define[I, O any](name, version string, fn func(context.Context, I) (O, error)) *Definition[I, O] {
	d := definition{name: name, version: version, inputType: typeName[I](), resultType: typeName[O]()}
	if fn != nil {
		d.invoke = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input I
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, fmt.Errorf("bonnie: decode workflow input: %w", err)
			}
			result, err := fn(ctx, input)
			if err != nil {
				return nil, err
			}
			data, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("bonnie: encode workflow result: %w", err)
			}
			return data, nil
		}
	}
	return &Definition[I, O]{def: d}
}

func (d *Definition[I, O]) workflowDefinition() definition {
	if d == nil {
		return definition{}
	}
	return d.def
}

func typeName[T any]() string { return describeType(reflect.TypeFor[T]()) }
func describeType(t reflect.Type) string {
	if t.Name() != "" {
		return t.PkgPath() + "/" + t.Name()
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice:
		return t.Kind().String() + "(" + describeType(t.Elem()) + ")"
	case reflect.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), describeType(t.Elem()))
	case reflect.Map:
		return "map[" + describeType(t.Key()) + "]" + describeType(t.Elem())
	case reflect.Struct:
		var s strings.Builder
		s.WriteString("struct{")
		for f := range t.Fields() {
			s.WriteString(f.PkgPath + "/" + f.Name + ":" + describeType(f.Type) + ":" + string(f.Tag) + ";")
		}
		s.WriteString("}")
		return s.String()
	default:
		return t.String()
	}
}

// Attempt identifies one callback invocation. IdempotencyKey stays the same
// across retries and restarts. Number starts at one and increases per attempt.
type Attempt struct {
	Number         int    `json:"number"`
	IdempotencyKey string `json:"idempotency_key"`
}

// RetryPolicy bounds retry attempts and exponential backoff. MaxAttempts
// includes the first attempt. Zero delays permit immediate retry.
type RetryPolicy struct {
	MaxAttempts  int           `json:"max_attempts"`
	InitialDelay time.Duration `json:"initial_delay"`
	MaxDelay     time.Duration `json:"max_delay"`
}

type stepConfig struct {
	RepeatSafe bool          `json:"repeat_safe"`
	Retry      RetryPolicy   `json:"retry"`
	Timeout    time.Duration `json:"timeout"`
}

// StepOption configures a step. Options are part of its replay identity.
type StepOption func(*stepConfig)

// RepeatSafe permits the callback to run again after a failure or crash.
// Use it only when the external system deduplicates IdempotencyKey, or when
// repeating the action is safe without deduplication.
func RepeatSafe() StepOption { return func(c *stepConfig) { c.RepeatSafe = true } }

// WithRetry sets explicit retry limits. More than one attempt requires RepeatSafe.
func WithRetry(p RetryPolicy) StepOption { return func(c *stepConfig) { c.Retry = p } }

// WithAttemptTimeout bounds the context given to each callback. Callbacks
// must honor cancellation. A timeout cannot undo an external effect.
func WithAttemptTimeout(d time.Duration) StepOption { return func(c *stepConfig) { c.Timeout = d } }

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent prevents retries of an error. A nil error stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}
func isPermanent(err error) bool { var p permanentError; return errors.As(err, &p) }

func (c stepConfig) validate() error {
	if c.Retry.MaxAttempts < 1 || c.Retry.InitialDelay < 0 || c.Retry.MaxDelay < 0 || c.Timeout < 0 {
		return errors.New("bonnie: invalid step options")
	}
	if c.Retry.MaxAttempts > 1 && !c.RepeatSafe {
		return errors.New("bonnie: retries require RepeatSafe")
	}
	if c.Retry.MaxDelay > 0 && c.Retry.MaxDelay < c.Retry.InitialDelay {
		return errors.New("bonnie: retry maximum delay is less than initial delay")
	}
	return nil
}
func (c stepConfig) delay(attempt int) time.Duration {
	d := c.Retry.InitialDelay
	if d == 0 {
		return 0
	}
	limit := c.Retry.MaxDelay
	if limit == 0 {
		limit = time.Duration(1<<63 - 1)
	}
	for i := 1; i < attempt && d < limit; i++ {
		if d > limit/2 {
			return limit
		}
		d *= 2
	}
	if d > limit {
		return limit
	}
	return d
}
