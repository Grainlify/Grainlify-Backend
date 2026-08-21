#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp payout_claims.go ../payout/chainconfig.go "$BAK/"
before=$(cat payout_claims.go ../payout/chainconfig.go | shasum | cut -d" " -f1)
restore_all() {
  cp "$BAK/payout_claims.go" ./; cp "$BAK/chainconfig.go" ../payout/
  [ "$before" = "$(cat payout_claims.go ../payout/chainconfig.go | shasum | cut -d' ' -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"
}
trap restore_all EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_519?sslmode=disable}"
run() { go test . -run 'TestClaims_|TestGetClaim_' -count=1 >/tmp/pc.log 2>&1 && go test ../payout/ -run TestChainConfig -count=1 >>/tmp/pc.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK/payout_claims.go" ./; cp "$BAK/chainconfig.go" ../payout/
  python3 -c "
import sys
f=open(sys.argv[1]).read()
if sys.argv[2] not in f: sys.exit(2)
open(sys.argv[1],'w').write(f.replace(sys.argv[2], sys.argv[3], 1))" "$2" "$3" "$4" || { echo "  SKIP      $1 — PATTERN MISSED"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: drop every new field ==="
cp "$BAK/payout_claims.go" ./
python3 -c "
f=open('payout_claims.go').read()
for k in ['\"contract_address\":      cc.ContractAddress,','\"network\":               cc.Network,','\"explorer_url_template\": cc.ExplorerURLTemplate,']:
    f=f.replace(k,'',1)
open('payout_claims.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "empty string instead of erroring on a NULL contract" ../payout/chainconfig.go 'if contract == nil || *contract == "" {' 'if false {'
mut "collapse the two chain errors into one"             ../payout/chainconfig.go 'ErrChainConfigIncomplete, chainID, missing)' 'ErrChainNotConfigured, chainID, missing)'
mut "return a populated config alongside the error"      ../payout/chainconfig.go 'return ChainConfig{}, fmt.Errorf("%w: chain %q is configured but missing %v. "+' 'return c, fmt.Errorf("%w: chain %q is configured but missing %v. "+'
mut "hardcode the symbol again"                          payout_claims.go 'fiber.Map{"symbol": cc.AssetSymbol, "decimals": decimals}' 'fiber.Map{"symbol": "USDC", "decimals": decimals}'
mut "serve the network as a URL"                         payout_claims.go '"network":               cc.Network,' '"network":               "https://fullnode.testnet.aptoslabs.com",'
mut "drop the frozen-address date"                       payout_claims.go '"claim_address_verified_at": a.verifiedAt,' ''
mut "collapse no_live_address into superseded"           payout_claims.go 'status = "no_live_address"' 'status = "superseded"'
mut "return the first of several again"                  payout_claims.go '	if len(claims) > 1 {' '	if false {'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
