#!/usr/bin/env bash
set -euo pipefail

command -v go >/dev/null || { echo "missing command: go" >&2; exit 2; }
command -v python3 >/dev/null || { echo "missing command: python3" >&2; exit 2; }

result=$(mktemp)
trap 'rm -f "$result"' EXIT

go test -tags integration -json ./test/... | tee "$result"
python3 - "$result" <<'PY'
import json
import sys

skipped = []
with open(sys.argv[1], encoding="utf-8") as stream:
    for line in stream:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get("Action") == "skip" and event.get("Test"):
            skipped.append(event["Test"])
if skipped:
    print("production acceptance forbids skipped integration tests:", file=sys.stderr)
    for name in sorted(set(skipped)):
        print(f"  - {name}", file=sys.stderr)
    raise SystemExit(1)
PY
