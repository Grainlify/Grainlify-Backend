#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp ./*.go "$BAK/"
before=$(cat ./*.go | shasum | cut -d" " -f1)
restore_all() {
  cp "$BAK"/*.go ./
  [ "$before" = "$(cat ./*.go | shasum | cut -d" " -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"
}
trap restore_all EXIT
run() { go test . -count=1 >/tmp/dbg-mut.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK"/*.go ./
  python3 -c "
import sys
f=open('dbguard.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('dbguard.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))" "$2" "$3" || { echo "  SKIP      $1 — PATTERN DID NOT MATCH"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: allow everything ==="
cp "$BAK"/*.go ./
python3 -c "
f=open('dbguard.go').read()
f=f.replace('func Check(what, dbURL string, args []string) error {','func Check(what, dbURL string, args []string) error {\n\tif true { return nil }',1)
open('dbguard.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "treat every host as local"          'func isLocal(host string) bool { return localHosts[strings.ToLower(host)] }' 'func isLocal(host string) bool { return true }'
mut "fail OPEN on an unparseable DB_URL" 'return "", fmt.Errorf("DB_URL is empty")' 'return "localhost", nil'
mut "drop the host-mismatch check"       '	if named != host {' '	if false {'
mut "accept the bare flag"               '			return "", true // present but with no value: seen, and will not match' '			return host, true'
mut "leak the URL into the refusal"      'what, host, fmt.Sprintf' 'what, dbURL, fmt.Sprintf'
mut "unix socket treated as remote"      'return "localhost", nil // unix socket is by definition this machine' 'return h, nil'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped mutation(s) never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
