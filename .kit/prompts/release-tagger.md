---
description: Tag and publish a release — goreleaser owns the artifacts, T-011 owns the checklist
---

Prepare, validate, and cut a release of BONNIE. The user's input, if any: $@

## Steps

1. **Fetch remote tags**: `git fetch --tags origin`, and read the latest:
   `git tag -l | sort -V | tail -5`

2. **Work the release checklist** — `docs/TASKS.md` T-011 is the source of
   truth; confirm each box:
   - `task release-check` (goreleaser check) and `task release-snapshot`
     (goreleaser build --snapshot --clean) must both succeed
   - All CI jobs green on `master`, including `boundary` (`gh run list`)
   - `CHANGELOG.md` has the release's changes under a section matching the
     version, in Keep-a-Changelog shape
   - Release notes must state the three claims — survives process death,
     parks indefinitely, reachable over HTTP — **and the limits** from
     `README.md`, just as plainly

3. **Determine the version** — BONNIE is pre-1.0, so SemVer gives:
   - **MINOR (0.X.0)**: new features, public API additions, breaking changes
     (pre-1.0 convention: breaking lands in the minor)
   - **PATCH (0.0.X)**: fixes, docs, internal work
   - Look at `git log <latest-tag>..HEAD --oneline`:
     `feat:` → MINOR · `fix:`/`docs:`/`chore:` → PATCH · anything touching
     exported signatures of `runtime/`, `channel/`, or `sandbox/` deserves a
     MINOR even without a `feat:` prefix

4. **Write the release notes** (goreleaser reads `CHANGELOG.md`):
   - Group: Added / Changed / Fixed / Removed
   - State the claims **and** the limits — a framework that hides its limits
     gets deployed into situations it cannot handle

5. **Tag and push**:
   - Tagging is a **human decision** — present the version and the notes, and
     wait for explicit confirmation
   - `git tag -a vX.Y.Z -m "..."` then `git push origin vX.Y.Z` — `release.yml`
     fires on `v*` and owns the artifacts; do not build or upload by hand

6. **Verify the release**:
   - Wait for the `release.yml` run to finish (`gh run watch`)
   - Install or download the artifact and run `bonnie version` — it must print
     the injected version, not `dev`
   - Check the published artifacts exist on the release page

7. **Close out**: tick T-011's boxes in `docs/TASKS.md` and move it to the
   shipped table; drop the `[Unreleased]` heading in `CHANGELOG.md` to the
   released version.

## Guidelines

- Never force-push or re-tag a version that was published
- If the checklist has a red box, stop — do not tag around it
- The version the binary prints comes from `-X main.version` at link time; if
  it prints `dev`, the artifacts are wrong — investigate, do not re-tag

$@
