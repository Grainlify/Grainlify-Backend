#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp payout_address.go "$BAK/"
before=$(cat payout_address.go | shasum | cut -d" " -f1)
restore_all() { cp "$BAK/payout_address.go" ./
  [ "$before" = "$(cat payout_address.go | shasum | cut -d' ' -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"; }
trap restore_all EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_086?sslmode=disable}"
# The filter must cover every test that guards this file, not just the ones that
# existed when the harness was written. A narrower filter reports SURVIVED for a
# mutation its own tests would have caught - the harness answering a narrower
# question than it was asked.
run() { go test . -run 'TestRegister_|TestIsDuplicateAddress' -count=1 >/tmp/ma.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK/payout_address.go" ./
  python3 -c "
import sys
f=open('payout_address.go').read()
if sys.argv[1] not in f: sys.exit(2)
open('payout_address.go','w').write(f.replace(sys.argv[1], sys.argv[2], 1))" "$2" "$3" || { echo "  SKIP      $1 — PATTERN MISSED"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: accept every registration ==="
cp "$BAK/payout_address.go" ./
python3 -c "
f=open('payout_address.go').read()
f=f.replace('func (h *PayoutAddressHandler) PostAddress(c *fiber.Ctx) error {','func (h *PayoutAddressHandler) PostAddress(c *fiber.Ctx) error {\n\tif true { return c.Status(201).JSON(fiber.Map{\"verified_at\":\"x\"}) }',1)
open('payout_address.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "drop the another-account check"        'WHERE chain_id = $1 AND address = $2 AND superseded_at IS NULL AND user_id <> $3`,' 'WHERE chain_id = $1 AND address = $2 AND superseded_at IS NULL AND user_id <> $3 AND false`,'
mut "also block SUPERSEDED duplicates"      'AND superseded_at IS NULL AND user_id <> $3`,' 'AND user_id <> $3`,'
mut "block the user reregistering their own" 'AND superseded_at IS NULL AND user_id <> $3`,' 'AND superseded_at IS NULL`,'
mut "let any signature through (conflict then leaks first)" 'derived, verr := auth.VerifyAptosSignature(addr, msg, body.Nonce, body.Signature, body.PublicKey)' 'derived, verr := "", error(nil); _ = msg'
mut "swallow the unique-index race"         'return err != nil && strings.Contains(err.Error(), "idx_contributor_addresses_one_account")' 'return false'
mut "refuse with no remedy"                 'If it is not, contact us — a payout ' 'Nope. '
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
