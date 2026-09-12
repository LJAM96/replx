#!/bin/zsh
# Automated P0 spike protocol checks (curl-level mechanics).
# Real-client behaviour (TV apps, seeking feel, subtitles) is recorded
# manually per docs/spike_runbook.md.
#
# Required env: CONTROL (default http://127.0.0.1:32400),
# ADMIN (default http://127.0.0.1:8080), SETUP_TOKEN, PLEX_TOKEN,
# PART (default /library/parts/11/file.mkv).
set -euo pipefail

CONTROL="${CONTROL:-http://127.0.0.1:32400}"
ADMIN="${ADMIN:-http://127.0.0.1:8080}"
PART="${PART:-/library/parts/11/file.mkv}"
: "${SETUP_TOKEN:?set SETUP_TOKEN}"
: "${PLEX_TOKEN:?set PLEX_TOKEN}"

pass=0; fail=0
check() { # name, command... (succeeds silently on pass)
  if "$@" >/dev/null 2>&1; then echo "PASS $1"; pass=$((pass+1)); else echo "FAIL $1"; fail=$((fail+1)); fi
}

echo "== readiness =="
READY="$(curl -s "$ADMIN/health/ready")"
echo "$READY" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d['status']=='ready', d; assert d.get('valkey') in ('ok','degraded'), d"
echo "PASS ready"

echo "== 307 shape =="
HDRS="$(mktemp)"; trap 'rm -f "$HDRS"' EXIT
curl -s -D "$HDRS" -o /dev/null -H "X-Plex-Token: $PLEX_TOKEN" -H "Range: bytes=0-99" "$CONTROL$PART"
grep -q "307" "$HDRS" && echo "PASS status 307" && pass=$((pass+1)) || { echo "FAIL status 307"; fail=$((fail+1)); }
grep -qi "cache-control: no-store" "$HDRS" && echo "PASS no-store" && pass=$((pass+1)) || { echo "FAIL no-store"; fail=$((fail+1)); }
LOC="$(grep -i '^location:' "$HDRS" | tr -d '\r' | cut -d' ' -f2)"
echo "Location: $(echo "$LOC" | python3 -c "import sys,urllib.parse; u=sys.stdin.read().strip(); q=urllib.parse.urlparse(u); print(q.hostname, '| token-present:', 'Token' in u)")"
echo "$LOC" | grep -q "X-Plex-Token=" && echo "PASS token query" && pass=$((pass+1)) || { echo "FAIL token query"; fail=$((fail+1)); }
case "$LOC" in
  *plex.example.com*) echo "FAIL location points at Replex hostname"; fail=$((fail+1));;
  https://*) echo "PASS https origin"; pass=$((pass+1));;
  *) echo "FAIL location scheme/host: $LOC"; fail=$((fail+1));;
esac

echo "== Range round-trip through redirect =="
CODE="$(curl -s -o /dev/null -w "%{http_code}" -L -H "Range: bytes=0-99" "$CONTROL$PART?X-Plex-Token=$PLEX_TOKEN" || echo "000")"
echo "followed status: $CODE (206/200 against a real origin with that part; 401/404 against fakes still proves redirect mechanics; 000 means the origin is unreachable from here)"

echo "== manifest paths redirect, not proxy =="
MCODE="$(curl -s -o /dev/null -w "%{http_code}" -H "X-Plex-Token: $PLEX_TOKEN" "$CONTROL/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1")"
[ "$MCODE" = "307" ] && echo "PASS manifest 307" && pass=$((pass+1)) || { echo "FAIL manifest status: $MCODE"; fail=$((fail+1)); }

echo "== trace ring has no token =="
RING="$(curl -s -H "Authorization: Bearer $SETUP_TOKEN" "$ADMIN/api/v1/spike/events")"
echo "$RING" | grep -q "$PLEX_TOKEN" && { echo "FAIL token in trace ring"; fail=$((fail+1)); } || { echo "PASS ring redacted"; pass=$((pass+1)); }

echo
echo "pass=$pass fail=$fail"
[ "$fail" = "0" ]
