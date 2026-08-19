#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp address.go "$BAK/"; trap 'cp "$BAK/address.go" ./address.go; rm -rf "$BAK"' EXIT
run() { go test . -count=1 >/tmp/pa-mut.log 2>&1; }
k=0; s=0; declare -a SURV=()
mut() { cp "$BAK/address.go" ./address.go
  python3 -c "
import sys
f=open('address.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('address.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))" "$2" "$3" || { echo "  SKIP      $1"; return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: accept everything ==="
cp "$BAK/address.go" ./address.go
python3 -c "
f=open('address.go').read()
f=f.replace('func Validate(raw string) (string, error) {','func Validate(raw string) (string, error) {\n\tif true { return raw, nil }',1)
open('address.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "drop the reserved-range check"        '	if n.Cmp(big.NewInt(ReservedBelow)) < 0 {' '	if false {'
mut "off-by-one: allow 0xf through"        'const ReservedBelow = 0x10' 'const ReservedBelow = 0x0f'
mut "check the raw string, not the value"  '	n, ok := new(big.Int).SetString(c[2:], 16)' '	n, ok := new(big.Int).SetString(strings.TrimLeft(raw, "0x"), 16)'
mut "skip lowercasing"                     '	body := strings.ToLower(s[2:])' '	body := s[2:]'
mut "skip zero-padding"                    '	return "0x" + strings.Repeat("0", 64-len(body)) + body, nil' '	return "0x" + body, nil'
mut "accept over-long addresses"           '	if len(body) > 64 {' '	if false {'
mut "accept a missing 0x prefix"           '	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {' '	if false {'
mut "return a value alongside an error"    '		return "", fmt.Errorf("%w: %q", ErrNotHex, raw)' '		return raw, fmt.Errorf("%w: %q", ErrNotHex, raw)'
echo ""
echo "=== $k killed, $s survived ==="
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
