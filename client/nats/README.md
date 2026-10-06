# NATS client

This package uses JetStream only. The caller supplies and owns the NATS
connection. The client does not execute agents or store run state.

```go
import (
    "context"

    bonnienats "github.com/mark3labs/bonnie/client/nats"
    "github.com/mark3labs/bonnie/runtime"
    gonats "github.com/nats-io/nats.go"
)

func work(ctx context.Context, nc *gonats.Conn) error {
    c, err := bonnienats.New(nc, bonnienats.Config{
        TaskSubject: "tasks", ResultSubject: "results", AnswerSubject: "answers",
        CreateStream: true, // Use stable subject-derived stream and consumer names.
    })
    if err != nil {
        return err
    }
    _, err = c.Submit(ctx, bonnienats.Task{
        TaskID: "request-123", Text: "Ask the operator for a location",
    })
    if err != nil {
        return err
    }
    return c.Consume(ctx, func(ctx context.Context, o bonnienats.Outcome) error {
        if o.State == runtime.RunWaiting {
            // Get this response from the application or operator.
            _, err := c.Answer(ctx, o, []runtime.InputResponse{{Text: "London"}})
            return err
        }
        // Store the outcome here. Return an error if storage fails.
        return nil
    })
}
```

## Resources and delivery

The operator or channel must create an input stream that covers `tasks` and
`answers.*`. `CreateStream` permits creation of the result stream only. Without
it, the result stream must exist. New creates the durable result pull consumer
if it does not exist. It does not change existing streams or consumers.

ResultStream and ResultConsumer are optional. Their defaults are stable names
from the exact ResultSubject. DefaultResultStreamName and
DefaultResultConsumerName expose these names for operators. Defaults do not
change the caller's configuration or enable stream creation. Set ResultStream
to bind an existing stream with a different name.

Clients using the default consumer share one main-service processing group and
keep acknowledgement state after restart. Changing a consumer name can replay
retained results. Use a separate result consumer for each application that needs all results.
Clients with the same consumer share its work. Consume reads all results on the
configured subject, not only the tasks that this client submitted. The handler
must select the task IDs that its application needs.

Consume acknowledges a result only after handler success. It sends heartbeats
while the handler runs. Handler failure sends a negative acknowledgement with a
one-second delay and returns the error. Call Consume again to retry. Decode
failures also return without discarding the result. An operator must correct
invalid messages to stop repeated failures. Handlers must respect context
cancellation; the client cannot stop a handler that ignores its context.

Delivery is at least once. Results can repeat. Separate workers can execute the
same task again. Use TaskID, AttemptID, RunID, and suspension ToolCallID to track
the required application state. A successful publish receipt confirms broker
storage, not execution. Broker errors are returned to the caller.

Reuse TaskID on the same TaskSubject only to retry the same task. Task publish
headers use a fixed prefix and a SHA-256 digest of TaskSubject and TaskID.
Answers use a deterministic identity from the full answer route (base and
WorkerID), RunID, and ToolCallID. These identities keep separate routes distinct
when they share a stream, because broker deduplication applies across the stream.
The first answer for that suspension and route wins within the stream duplicate
window; changing response text does not create a new answer. Broker deduplication
expires, so it is not an exactly-once guarantee.

## Raw protocol

The wire types are aliases of `channel/nats.Task`, `Result`, and `Answer`.
Outcome is an alias of Result. Task uses `task_id` (Go field `TaskID`), not `id`.
All messages use JSON version 1. Submit accepts Task.Version 0 or 1 and stores
version 1 without changing the caller's Task. It rejects all other versions.
Raw publishers must set version 1. Tasks go to TaskSubject; results go to
ResultSubject. Answers go only to `AnswerSubject + "." + WorkerID`. Answer checks
that WorkerID is a safe single token and that the result's answer_subject equals
that exact route. It also requires a waiting run and its suspension tool-call
ID. No reply subject or arbitrary route from a result is used.

These checks do not authenticate a worker. Use NATS permissions to restrict
publishers, consumers, and stream administration. Result errors, including
rejected input, are delivered to the handler as outcomes, not Consume errors.
