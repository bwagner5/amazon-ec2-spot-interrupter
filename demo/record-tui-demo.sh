#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

if ! command -v vhs >/dev/null 2>&1; then
  echo "vhs is required. Install from https://github.com/charmbracelet/vhs" >&2
  exit 1
fi

MOCK_LOG="${TMPDIR:-/tmp}/mockaws-demo.log"
: > "$MOCK_LOG"

go run ./cmd/mockaws --scale small --listen 127.0.0.1:18080 >"$MOCK_LOG" 2>&1 &
MOCK_PID=$!

SHIM_DIR="$(mktemp -d "${TMPDIR:-/tmp}/spot-demo-bin.XXXXXX")"
cat >"$SHIM_DIR/ec2-spot-interrupter" <<EOF
#!/usr/bin/env bash
set -euo pipefail
cd "$ROOT_DIR"
exec go run ./cmd "\$@"
EOF
chmod +x "$SHIM_DIR/ec2-spot-interrupter"

cleanup() {
  kill "$MOCK_PID" >/dev/null 2>&1 || true
  wait "$MOCK_PID" 2>/dev/null || true
  rm -rf "$SHIM_DIR"
}
trap cleanup EXIT

for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:18080/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

PATH="$SHIM_DIR:$PATH" ENDPOINT="http://127.0.0.1:18080" vhs demo/tui-demo.tape

echo "Wrote demo/tui-demo.gif"
