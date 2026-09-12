---
description: End-to-end live smoke test — real model, real journal, park and resume across processes
---

Run BONNIE's live smoke test: the whole claim against a real provider — a run survives process death, parks for a human, and answers over HTTP.

## Before you start

1. **A provider key is required** — without one, everything below skips, and the smoke test is vacuous. Stop and ask for the key instead of reporting success:
   ```bash
   export ANTHROPIC_API_KEY=sk-ant-...   # or OPENAI_API_KEY; the live
                                         # suites default to opencode/kimi-k2.5
   ```
2. Run from a scratch directory if you can — the examples write `.bonnie/` journals.

## Steps

1. **The integration suites** (build-tagged, skip without a key):
   ```bash
   task test-live
   ```
   Verify: `TestLiveSuspendAndResume` in `runtime` RAN (not `ok` with skips alone — read the verbose output with `go test -race -tags integration -v ./runtime`), and the sandbox suite's backend cases ran. `task test-live` covers `./runtime ./sandbox`; with `BONNIE_TEST_SANDBOX=microsandbox` the sandbox suite drives the microVM backend.

2. **Park and resume across a real process exit**:
   ```bash
   go run ./examples/hitl-restart -phase ask
   # process exits, run is parked on disk
   bonnie runs list --journal .bonnie --state waiting
   go run ./examples/hitl-restart -phase answer -answer "eu-west-1"
   ```
   Verify: the first phase ends with the run `waiting` and the process gone; the second phase prints a **completed** run that remembers the question. Inspect the journal if anything looks wrong:
   `bonnie runs show --journal .bonnie hitl-1 --json`.

3. **Reachable over HTTP**:
   ```bash
   task dev -- serve --journal /tmp/bonnie-smoke --addr :8080 &
   curl -s localhost:8080/runs -d '{"text":"Deploy the app. Ask me which region first."}'
   ```
   Verify, in order:
   - `POST /runs` returns a run ID and, if it parked, `state:"waiting"` with a prompt
   - `curl -sN localhost:8080/runs/<id>/stream` streams NDJSON events
   - Reconnect with `?cursor=<last-seen>` — no gap, no duplicate
   - `curl -s localhost:8080/runs/<id>/respond -d '{"responses":[{"text":"eu-west-1"}]}'` completes the run
   - Kill the server with SIGINT — it must drain gracefully, not die mid-turn

4. **Check the invariants held**:
   - `bonnie runs list --journal /tmp/bonnie-smoke` — no `bonnie.`-prefixed
     reserved runs leak into the operator listing (invariant 8)
   - A resumed run's timeline shows the question, the suspension, and the
     answer with no orphaned tool calls (invariants 3–4)
   - The parked phase held no compute — nothing but the journal on disk

5. **Report**: which suites ran (and against which model/backend), which phases
   passed, and any skip — a skip in this smoke test is a finding, not an OK.

## Guidelines

- A green test run with every test **skipped** is a failed smoke test — count the runs, not the `ok`s
- Clean the scratch journals when done (`rm -rf /tmp/bonnie-smoke .bonnie`)
- Never run the smoke test with a journal that holds a real conversation

$@
