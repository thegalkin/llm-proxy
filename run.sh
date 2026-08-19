#!/bin/bash
# llm-proxy launcher.
#
# Rebuild binary on first run whenever it would be stale:
#   1. Foreign-arch build (e.g. Mach-O from macOS on Linux, or ELF from
#      Linux on macOS) — syncthing pulled a build for the wrong OS.
#   2. Source-staleness — any .go file under cmd/ or internal/ (plus
#      go.mod / go.sum) is newer than the on-disk binary. Catches
#      commits that change code without changing arch.
# Both paths are idempotent; skipping happens when nothing needs to
# be done.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

# Detect native executable format for the current OS
case "$(uname -s)" in
  Linux)  native_fmt="ELF"  ;;
  Darwin) native_fmt="Mach-O" ;;
  *)
    echo "[run.sh] unknown OS: $(uname -s)" >&2
    exit 1
    ;;
esac

need_rebuild=0
reason=""

# (1) Foreign-arch binary?
if ! file llm-proxy 2>/dev/null | grep -q "$native_fmt"; then
  need_rebuild=1
  reason="binary is not a $native_fmt"
fi

# (2) Sources newer than binary? Skip silently on a fresh checkout where
# cmd/ and internal/ don't exist yet (defensive — main branch always has them).
if [ "$need_rebuild" -eq 0 ] && { [ -d cmd ] || [ -d internal ]; }; then
  bin_mtime=$(stat -c '%Y' llm-proxy 2>/dev/null || echo 0)
  src_mtime=$(find cmd internal \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -type f -printf '%T@\n' 2>/dev/null | sort -nr | head -1 | cut -d. -f1)
  if [ -n "${src_mtime:-}" ] && [ "${src_mtime:-0}" -gt "$bin_mtime" ]; then
    need_rebuild=1
    reason="sources are newer than binary (src=${src_mtime} > bin=${bin_mtime})"
  fi
fi

if [ "$need_rebuild" -eq 1 ]; then
  echo "[run.sh] $reason; rebuilding for $(uname -m)..." >&2
  export PATH="$HOME/.local/go/bin:$PATH"
  # systemd unit sets PrivateTmp=true + ProtectSystem=strict, which makes
  # the default GOCACHE (= $HOME/.cache/go-build) read-only. Redirect the
  # build cache into a writable location under TMPDIR (/tmp under the
  # service-private namespace).
  export GOCACHE="${TMPDIR:-/tmp}/go-build-llm-proxy"
  mkdir -p "$GOCACHE"
  go build -trimpath -ldflags="-s -w" -o llm-proxy ./cmd/llm-proxy >&2
fi

export ENV_FILE="$DIR/.env"
exec "$DIR/llm-proxy"
