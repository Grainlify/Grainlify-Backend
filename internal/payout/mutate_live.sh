#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp ./*.go "$BAK/"
before=$(cat ./*.go | shasum | cut -d" " -f1)
restore_all() { cp "$BAK"/*.go ./
  [ "$before" = "$(cat ./*.go | shasum | cut -d' ' -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"; }
trap restore_all EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_lr?sslmode=disable}"
run() { go test . -run 'TestLiveReader' -count=1 >/tmp/lr.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK"/*.go ./
  python3 -c "
import sys
f=open('live.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('live.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))" "$2" "$3" || { echo "  SKIP      $1 — PATTERN MISSED"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: return a fixed deadline without reading ==="
cp "$BAK"/*.go ./
python3 -c "
f=open('live.go').read()
f=f.replace('func (r *LiveReader) Deadline(ctx context.Context, settlementID uuid.UUID) (time.Time, error) {','func (r *LiveReader) Deadline(ctx context.Context, settlementID uuid.UUID) (time.Time, error) {\n\tif true { return time.Unix(1800000000,0).UTC(), nil }',1)
open('live.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
# The mutation declares its own state rather than requiring a field in
# production. A struct field that exists only so a harness can mutate it is dead
# code with an invitation attached.
mut "cache the deadline after the first read" 'func (r *LiveReader) Deadline(ctx context.Context, settlementID uuid.UUID) (time.Time, error) {' 'var mutCache int64

func (r *LiveReader) Deadline(ctx context.Context, settlementID uuid.UUID) (time.Time, error) {
	if mutCache != 0 { return time.Unix(mutCache, 0).UTC(), nil }
	defer func() { mutCache = 1 }()'
mut "serve an unpublished settlement anyway"  'if publishedTx == nil || escrow == "" {' 'if false {'
mut "fall back when no endpoint is set"       'nodeURL, err := chainread.EndpointFor(name)' 'nodeURL, err := "https://fullnode.testnet.aptoslabs.com", error(nil); _ = name'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
