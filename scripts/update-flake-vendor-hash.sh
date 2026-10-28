#!/usr/bin/env bash
# Refresh the go-modules hash (vendorHash) in nix/bonnie.nix.
#
# The Nix build fetches the module tree of the *pinned release* in a
# fixed-output derivation, and vendorHash pins that tree. It has to move
# whenever the pinned release ships different dependencies. Build with a fake
# hash, read the hash Nix reports as expected out of the failure, write it
# back, then prove the build goes through.
#
# The `nix-release-pin` job in .github/workflows/release.yml runs this after
# bumping the release pin; run it by hand when you bump the pin yourself:
#
#   scripts/bump-flake-release-pin.sh v1.2.3
#   nix flake lock
#   scripts/update-flake-vendor-hash.sh
#
# Any extra arguments are passed through to `nix build`, for example to work
# against a local nixpkgs:
#
#   scripts/update-flake-vendor-hash.sh --override-input nixpkgs /path/to/nixpkgs
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PKG_NIX="$ROOT/nix/bonnie.nix"
FAKE_HASH="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

if [[ "$(grep -cE 'vendorHash[[:space:]]*=' "$PKG_NIX")" != "1" ]]; then
  echo "error: $PKG_NIX must have exactly one vendorHash line" >&2
  exit 1
fi

set_hash() {
  sed -i -E "s|(vendorHash[[:space:]]*=[[:space:]]*\")[^\"]*(\"[[:space:]]*;)|\1$1\2|" "$PKG_NIX"
}

OLD_HASH="$(grep -oE 'vendorHash[[:space:]]*=[[:space:]]*"[^"]*"' "$PKG_NIX" | cut -d'"' -f2)"
set_hash "$FAKE_HASH"

log="$(nix build --no-link "$@" "$ROOT#bonnie" 2>&1 || true)"
hash="$(grep -oE 'got:[[:space:]]+sha256-[A-Za-z0-9+/=]+' <<<"$log" | head -1 | awk '{print $2}')"
if [[ -z "$hash" ]]; then
  # Leave the file as it was, not with the fake hash in it.
  set_hash "$OLD_HASH"
  echo "error: could not read the expected module hash from the build output:" >&2
  echo "$log" | tail -20 >&2
  exit 1
fi

set_hash "$hash"

# Prove the flake builds with the new hash before announcing it.
nix build --no-link "$@" "$ROOT#bonnie"

echo "vendorHash is now $hash"
