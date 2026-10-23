#!/usr/bin/env bash
# Run the full Linux suite without user configuration or Windows interop.
set -euo pipefail
export GOMODCACHE="$(go env GOMODCACHE)" GOCACHE="$(go env GOCACHE)"
test_root=$(mktemp -d /tmp/nano-test.XXXXXX)
trap 'rm -rf "$test_root"' EXIT
export HOME="$test_root/home" NANO_HOME="$test_root/nano" USERPROFILE="$test_root/profile"
export XDG_CONFIG_HOME="$test_root/config" XDG_RUNTIME_DIR="$test_root/run" NANO_TEST_NO_WINDOWS=1
mkdir -p "$HOME" "$NANO_HOME" "$USERPROFILE" "$XDG_CONFIG_HOME" "$XDG_RUNTIME_DIR"
if [ "${1:-}" = "--" ]; then shift; "$@"; else make "${@:-test}"; fi
