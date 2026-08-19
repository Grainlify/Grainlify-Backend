#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
BAK=$(mktemp -d); cp resolve.go digest.go build.go dryrun.go "$BAK/"
trap 'cp "$BAK"/*.go ./; rm -rf "$BAK"' EXIT
export TEST_DB_URL="${TEST_DB_URL:-postgres://postgres@localhost:5432/grainlify_m79?sslmode=disable}"
run() { go test . -count=1 >/tmp/payout-mut.log 2>&1; }
k=0; s=0; declare -a SURV=()
mut() { cp "$BAK"/*.go ./
  python3 -c "
import sys
f=open(sys.argv[1]).read()
if sys.argv[2] not in f: sys.exit(2)
open(sys.argv[1],'w').write(f.replace(sys.argv[2], sys.argv[3], 1))" "$2" "$3" "$4" || { echo "  SKIP      $1 — PATTERN DID NOT MATCH"; skipped=$((skipped+1)); return; }
  if run; then echo "  SURVIVED  $1"; s=$((s+1)); SURV+=("$1"); else echo "  killed    $1"; k=$((k+1)); fi
}
# A SKIP is not a kill. A harness that reports "all killed" while some mutations
# never applied is the same defect as a suite that skips and prints ok.
skipped=0
echo "=== control: skip the digest and acknowledgement checks entirely ==="
cp "$BAK"/*.go ./
python3 -c "
f=open('build.go').read()
f=f.replace('	if ack.InputDigest != fresh.InputDigest {','	if false {',1)
f=f.replace('	if ack.ExcludedTotalMinor == nil || ack.ExcludedTotalMinor.Cmp(fresh.ExcludedTotalMinor) != 0 {','	if false {',1)
open('build.go','w').write(f)"
run && { echo "  CONTROL SURVIVED"; exit 1; }
echo "  control killed"; echo ""
echo "=== real mutations ==="
mut "drop the input-digest check"        build.go '	if ack.InputDigest != fresh.InputDigest {' '	if false {'
mut "drop the acknowledgement check"     build.go '	if ack.ExcludedTotalMinor == nil || ack.ExcludedTotalMinor.Cmp(fresh.ExcludedTotalMinor) != 0 {' '	if false {'
mut "accept any non-nil acknowledgement" build.go 'ack.ExcludedTotalMinor.Cmp(fresh.ExcludedTotalMinor) != 0' 'false'
mut "digest only the payable members"    digest.go '	for _, m := range members {' '	for _, m := range members { if m.Outcome != OutcomePayable { continue };'
mut "make field boundaries unrecoverable" digest.go '"%s\n%d:%s\n%d:%s\n%s\n"' '"%s%s%s%s"'
mut "drop the login from the digest"     digest.go 'len(m.GitHubLogin), m.GitHubLogin,' '0, "",'
mut "treat a missing github account as payable" resolve.go 'case m.GitHubLogin == "":' 'case false:'
mut "treat a missing address as payable" resolve.go 'case m.ClaimAddress == "":' 'case false:'
mut "silently drop excluded members"     resolve.go '		out = append(out, m)' '		if m.Outcome == OutcomePayable || amt.Sign() <= 0 { out = append(out, m) }'
mut "default an unknown pool to contributor" build.go '		return 0, fmt.Errorf("%w: %q (expected \"contributor\" or \"maintainer\")", ErrUnknownPool, p)' '		return chain.PoolKindContributor, nil'
mut "allow a second root"                build.go 'if exists {' 'if false {'
mut "let the dry run create the salt"    dryrun.go '	members, err := Resolve(ctx, pool, s)' '	_ = salt.Create(ctx, pool, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", s.SettlementID)\n	members, err := Resolve(ctx, pool, s)'
mut "excluded amounts counted as leaf total" dryrun.go 'r.ExcludedTotalMinor.Add(r.ExcludedTotalMinor, m.AmountMinor)' 'r.LeafTotalMinor.Add(r.LeafTotalMinor, m.AmountMinor)'
echo ""
echo "=== $k killed, $s survived, $skipped skipped ==="
[ "$skipped" -gt 0 ] && { echo "REFUSING TO REPORT: $skipped mutation(s) never applied, so this run proves less than it appears to."; exit 1; }
[ "$s" -gt 0 ] && { printf 'SURVIVED: %s\n' "${SURV[@]}"; exit 1; }
exit 0
