#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp ./*.go "$BAK/"
before=$(cat ./*.go | shasum | cut -d" " -f1)
restore_all() { cp "$BAK"/*.go ./
  [ "$before" = "$(cat ./*.go | shasum | cut -d' ' -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"; }
trap restore_all EXIT
run() { go test . -count=1 >/tmp/cr.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK"/*.go ./
  python3 -c "
import sys
f=open('aptos.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('aptos.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))" "$2" "$3" || { echo "  SKIP      $1 — PATTERN MISSED"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: every view returns a fixed value ==="
cp "$BAK"/*.go ./
python3 -c "
f=open('aptos.go').read()
f=f.replace('func (c *Client) view(ctx context.Context, function string, args []any) ([]json.RawMessage, error) {','func (c *Client) view(ctx context.Context, function string, args []any) ([]json.RawMessage, error) {\n\tif true { return []json.RawMessage{json.RawMessage(\`\"1\"\`)}, nil }',1)
open('aptos.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "treat a Move abort as a node failure"   'if ab := moveAbortRe.Find(raw); ab != nil {' 'if false {'
mut "treat every non-200 as an abort"        'if ab := moveAbortRe.Find(raw); ab != nil {' 'if true { ab := []byte("x");'
mut "discard the upstream status and body"   'return nil, fmt.Errorf("%w: status %d: %s", ErrNodeFailed, res.StatusCode, string(raw))' 'return nil, ErrNodeFailed'
mut "accept an unquoted u64 via float"      'var s string' 'var fl float64; if json.Unmarshal(out[0], &fl) == nil { return int64(fl), nil }; var s string'
mut "fall back to a public node when unset"  'return "", fmt.Errorf("%w: %s is not set, so no node can be reached", ErrNoEndpoint, envVarName)' 'return "https://fullnode.testnet.aptoslabs.com", nil'
mut "accept an empty view result"            'if len(out) == 0 {' 'if false {'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
