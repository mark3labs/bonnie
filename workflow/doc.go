// Package workflow runs durable Go workflow functions against a BONNIE
// journal. A workflow replays from the start after every durable wait. Step
// callbacks with saved outcomes do not run during replay.
//
// Workflow code must be deterministic. Put effects, clocks, randomness, and
// network calls in Step callbacks. Use Sleep and Wait for durable waits.
// Keep one goroutine per workflow function, do not call workflow operations
// from callbacks, and never recover the internal parking panic. Deferred
// functions run when the workflow parks; they must not have external effects.
//
// Definitions and JSON schemas are immutable within a version. Register a
// new version for changed code, and retain old versions for unfinished runs.
// All input, results, step values, and signals must support JSON encoding.
// JSON does not preserve Go error types; saved errors replay as error text.
//
// A saved pending callback is not proof that an external action happened.
// Unsafe callbacks with an unknown outcome block for explicit reconciliation.
// RepeatSafe is an explicit promise that repeating an action is safe. Its
// stable Attempt.IdempotencyKey should be sent to external systems that
// support deduplication. Attempt.Number is not an idempotency key.
//
// One Service owns the workflow namespace in one journal. The service does
// not supply distributed locks, leases, or exactly-once external effects.
// Reserved workflow run IDs are separate from conversation runs. The journal
// belongs to the host; stop the service before closing it.
package workflow
