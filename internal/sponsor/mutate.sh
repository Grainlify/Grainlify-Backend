#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp ./*.go "$BAK/"
before=$(cat ./*.go | shasum | cut -d" " -f1)
restore_all() { cp "$BAK"/*.go ./
  [ "$before" = "$(cat ./*.go | shasum | cut -d' ' -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
  rm -rf "$BAK"; }
trap restore_all EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_sp?sslmode=disable}"
run() { go test . -count=1 >/tmp/sp.log 2>&1; }
k=0; s=0; skipped=0; declare -a SURV=()
mut() { cp "$BAK"/*.go ./
  python3 -c "
import sys
f=open(sys.argv[1]).read()
if sys.argv[2] not in f: sys.exit(2)
open(sys.argv[1],'w').write(f.replace(sys.argv[2], sys.argv[3], 1))" "$2" "$3" "$4" || { echo "  SKIP      $1 — PATTERN MISSED"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
echo "=== control: every guard passes ==="
cp "$BAK"/*.go ./
python3 -c "
f=open('guard.go').read()
f=f.replace('func (g *Guard) CheckBalance(balanceOctas int64) error {','func (g *Guard) CheckBalance(balanceOctas int64) error {\n\tif true { return nil }',1)
f=f.replace('func (g *Guard) CheckRate(ctx context.Context, userID uuid.UUID) error {','func (g *Guard) CheckRate(ctx context.Context, userID uuid.UUID) error {\n\tif true { return nil }',1)
open('guard.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "round the per-claim cost"              guard.go 'const GasOctasPerClaim = 14_920 * 100' 'const GasOctasPerClaim = 15_000 * 100'
mut "drop the safety factor"                guard.go 'const SafetyFactor = 3' 'const SafetyFactor = 1'
mut "alarm may sit below the floor"         guard.go 'if dynamic < HardFloorOctas {' 'if false {'
mut "check the balance AFTER spending"      guard.go 'if balanceOctas-GasOctasPerClaim < HardFloorOctas {' 'if balanceOctas < HardFloorOctas {'
mut "count refusals toward the rate limit"  guard.go "AND outcome = 'submitted' AND created_at" "AND created_at"
mut "off-by-one in the rate limit"          guard.go 'if n >= MaxSponsorshipsPerHour {' 'if n > MaxSponsorshipsPerHour {'
mut "hide the floor from the message"       guard.go 'ErrLowBalance, aptString(balanceOctas), aptString(HardFloorOctas))' 'ErrLowBalance, aptString(balanceOctas), "some")'
mut "fall back when the key is unset"       key.go 'return nil, fmt.Errorf("%w: %s is not set. Claims cannot be sponsored, and this "+' 'return &Signer{priv: make([]byte, 64)}, error(nil); _ = fmt.Errorf("%w: %s "+'
mut "put the key in the error"              key.go 'return nil, fmt.Errorf("%w: %s is not hexadecimal", ErrBadSponsorKey, SponsorKeyEnv)' 'return nil, fmt.Errorf("%w: %s is not hexadecimal: %s", ErrBadSponsorKey, SponsorKeyEnv, raw)'
mut "treat an unreachable node as an abort" simulate.go 'return Simulation{}, fmt.Errorf("%w: %v", ErrSimulationUnavailable, err)' 'return Simulation{Success: false}, nil'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped never applied."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
