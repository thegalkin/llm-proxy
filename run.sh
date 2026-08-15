#!/bin/bash
# llm-proxy launcher.
#
# Reb binary on first run if syncthing pulled a foreign-arch build
# (e.g. Mach-O from macOS on Linux, or ELF from Linux on macOS),
# then execs it. Idempotent — once the binary matches the native
# format, the build step is skipped.
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

# Rebuild if the on-disk binary doesn't match the native format
if ! file llm-proxy 2>/dev/null | grep -q "$native_fmt"; then
  echo "[run.sh] llm-proxy is not a $native_fmt binary; rebuilding for $(uname -m)..." >&2
  export PATH="$HOME/.local/go/bin:$PATH"
  go build -trimpath -ldflags="-s -w" -o llm-proxy ./cmd/llm-proxy >&2
fi

export ENV_FILE="$DIR/.env"
exec "$DIR/llm-proxy"
