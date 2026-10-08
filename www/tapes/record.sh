#!/usr/bin/env bash
# Record a real CLI session without changing the operator's agent files.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
for tool in go vhs ffmpeg; do
  command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
done
if [ -z "${OPENAI_API_KEY:-}" ]; then
  echo 'Set OPENAI_API_KEY for the live openai/gpt-6.1-sol recording.' >&2
  exit 1
fi
workspace="$(mktemp -d)"
cleanup() {
  # Remove temporary agent state and any logs after the recording finishes.
  rm -rf "$workspace"
}
trap cleanup EXIT
export BONNIE_DEMO_DIR="$workspace"
export BONNIE_DEMO_BIN="$workspace/bin"
mkdir -p "$BONNIE_DEMO_BIN"
export PATH="$BONNIE_DEMO_BIN:$PATH"
# Use the checkout CLI, with a released scaffold dependency. No module replace.
go build -ldflags '-X main.version=v0.18.0' -o "$BONNIE_DEMO_BIN/bonnie" "$repo_root/cmd/bonnie"

# Warm dependency/build caches off camera; the filmed tree is still created live.
(
  cd "$workspace"
  "$BONNIE_DEMO_BIN/bonnie" init warmup --model openai/gpt-6.1-sol
  cd warmup
  go mod tidy
  "$BONNIE_DEMO_BIN/bonnie" build --output "$workspace/warmup-agent"
)
vhs_bin="${VHS_BIN:-vhs}"
export BONNIE_DEMO_GIF="$repo_root/www/public/quick-start.gif"
cd "$repo_root/www/tapes"
if command -v ttyd >/dev/null; then
  "$vhs_bin" quick-start.tape --output "$BONNIE_DEMO_GIF"
elif command -v nix-shell >/dev/null; then
  # Make ttyd available without changing the operator's profile.
  export BONNIE_VHS_BIN="$vhs_bin"
  nix-shell -p ttyd --run '"$BONNIE_VHS_BIN" quick-start.tape --output "$BONNIE_DEMO_GIF"'
else
  echo 'Install ttyd before recording.' >&2
  exit 1
fi
test -s "$BONNIE_DEMO_GIF" || { echo 'VHS produced no GIF. Check the VHS/ffmpeg versions.' >&2; exit 1; }
if command -v gifsicle >/dev/null; then
  gifsicle -O3 ../public/quick-start.gif -o ../public/quick-start.gif
fi
