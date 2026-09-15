#!/usr/bin/env bash
#
# Extract one release's section from CHANGELOG.md.
#
# goreleaser builds its release body from commit subjects, which publishes a
# commit list instead of the notes. This script slices the section for a
# version out of CHANGELOG.md so `goreleaser release --release-notes` can
# publish the real thing: the three claims and the limits.
#
# Usage:
#   scripts/release-notes.sh v0.5.0 [CHANGELOG.md]
#
# The version accepts a leading `v`; the changelog headings do not carry one.
# Exits non-zero, naming the missing heading, when the section is absent —
# a release must fail loudly rather than publish an empty body.

set -euo pipefail

if [ $# -lt 1 ]; then
	echo "usage: $0 <version> [changelog]" >&2
	exit 2
fi

version="${1#v}"
changelog="${2:-CHANGELOG.md}"

if [ ! -f "$changelog" ]; then
	echo "release-notes: no such file: $changelog" >&2
	exit 1
fi

# Headings are `## [0.5.0] — DATE`. Match the bracketed version exactly, so
# `0.5.0` never matches `0.5.0-rc1` or `10.5.0`, and stop at the next `## [`.
notes=$(awk -v want="$version" '
	/^## \[/ {
		# Pull the text between the first [ and ].
		line = $0
		sub(/^## \[/, "", line)
		sub(/\].*$/, "", line)
		if (line == want) { found = 1; next }
		if (found) { exit }
		next
	}
	found { print }
' "$changelog")

# Trim leading and trailing blank lines.
notes=$(printf '%s\n' "$notes" | sed -e '/./,$!d' | tac | sed -e '/./,$!d' | tac)

if [ -z "$notes" ]; then
	echo "release-notes: CHANGELOG.md has no section '## [$version]'." >&2
	echo "release-notes: add one before tagging v$version; the release body" >&2
	echo "release-notes: must state the three claims and the limits." >&2
	exit 1
fi

printf '%s\n' "$notes"
