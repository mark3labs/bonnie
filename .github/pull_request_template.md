## What changed and why

(Describe the change and the reasoning behind it.)

## Checklist

- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` passes
- [ ] No new direct import of `kit/internal/*`, and no import of `charm.land/fantasy` or `internal/fakemodel` outside test code
      (`task lint` — `depguard` is the gate; the `.kit/extensions/kit-boundary.go`
      extension only guards edits made through Kit in your checkout)
- [ ] Every exported symbol has a godoc comment
- [ ] Durability changes have a test that uses a second `Runner` sharing only the journal

## Related issue

(Link to the issue this closes: Closes #123)
