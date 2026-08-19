#!/usr/bin/env bash
# Mutation harness for the salt package.
#
# Every mutation reintroduces a defect the design exists to prevent. The control
# is certified lethal by construction (it makes the salt a constant).
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp salt.go "$BAK/"; trap 'cp "$BAK/salt.go" ./salt.go; rm -rf "$BAK"' EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_m79?sslmode=disable}"
run() { go test . -count=1 >/tmp/salt-mut.log 2>&1; }
k=0; s=0; declare -a SURV=()
mut() {
  cp "$BAK/salt.go" ./salt.go
  python3 -c "
import sys
f=open('salt.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('salt.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))
" "$2" "$3" || { echo "  SKIP      $1"; return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: make every salt the same constant ==="
cp "$BAK/salt.go" ./salt.go
python3 -c "
f=open('salt.go').read()
f=f.replace('if _, err := rand.Read(s); err != nil {','for i := range s { s[i] = 7 }\n\tif false {',1)
open('salt.go','w').write(f)"
if run; then echo "  CONTROL SURVIVED — harness not exercising the package"; exit 1; fi
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "drop the AAD binding (ciphertext portable between settlements)" \
  'func aad(settlementID uuid.UUID) []byte { return []byte("payout_event_salt:" + settlementID.String()) }' \
  'func aad(settlementID uuid.UUID) []byte { return nil }'
mut "do not zero the plaintext after use" \
  '	defer zero(s)

	h := &hasher{salt: s}' \
  '	h := &hasher{salt: s}'
mut "let an escaped hasher keep working" \
  '	if h.dead {
		return nil, ErrExpired
	}' \
  '	if false {
		return nil, ErrExpired
	}'
mut "report a destroyed salt as merely absent" \
  '	if destroyedAt != nil {
		return ErrDestroyed
	}' \
  '	if destroyedAt != nil {
		return ErrNotFound
	}'
mut "allow overwriting an existing salt" \
  '	if tag.RowsAffected() == 0 {
		// Either a live salt or a tombstone. Both mean "do not overwrite":' \
  '	if false {
		// Either a live salt or a tombstone. Both mean "do not overwrite":'
mut "destroy without requiring a reason" \
  '	if len(trimSpace(reason)) == 0 {
		return ErrNoReason
	}' \
  '	if false {
		return ErrNoReason
	}'
mut "rotation also re-wraps destroyed rows" \
  'FROM payout_event_salts WHERE destroyed_at IS NULL`)' \
  'FROM payout_event_salts`)'
mut "accept a key of any length (silent truncation)" \
  '	if len(raw) != 32 {' \
  '	if false {'
mut "hash the login without lowercasing (via a raw-salt shortcut)" \
  '		hh, err := chain.IdentityHash(login, h.salt)' \
  '		hh, err := chain.IdentityHash(login+"X", h.salt)'
echo ""
echo "=== $k killed, $s survived ==="
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
