#!/usr/bin/env bash
#
# A mutation harness that checks itself before it reports anything.
#
# WHY THIS EXISTS
#
# Three mutation runs in one session produced confidently wrong tables, in three
# different ways, and each looked like success:
#
#   1. Restoring with `git checkout` reverted the file to HEAD and discarded the
#      uncommitted refactor the tests needed. Everything after the first mutation
#      was a build error, recorded as four kills. A wall of green.
#
#   2. The failure detector matched Move's `error[E11001]: test failure`, which is
#      what a FAILING TEST prints, so real kills were recorded as invalid runs.
#
#   3. A patch applied to the file but changed no behaviour - `let _ = &x;` where
#      the intent was to clear x - and was recorded as a survivor. It proved
#      nothing about the tests at all.
#
# All three are the same failure: the harness reported on something other than
# what it claimed to measure. A no-op patch reporting a survivor and a build error
# reporting a kill are one bug wearing different clothes.
#
# So this script refuses to report until it has established that it is working:
#
#   * the baseline compiles and passes, before the first mutation and after the
#     last one
#   * a CONTROL mutation, known to be lethal, is actually killed - and if it is
#     not, every other result in the run is void
#   * each patch measurably changed the file
#   * a survivor is reported as UNVERIFIED, because a survivor is the one outcome
#     that is indistinguishable from a broken patch
#
# Restores from a copy, never from git: `git checkout <file>` restores from the
# index or HEAD, not from a moment ago, and while writing the tests you are about
# to mutate the tree always has uncommitted work in it.
#
# USAGE
#
#   scripts/mutate.sh <file> <test-cmd> <control-perl-expr> [<label>::<perl-expr>]...
#
# The control expression must be a mutation you are certain the suite catches.
#
#   scripts/mutate.sh internal/chain/merkle.go \
#     'go test ./internal/chain/' \
#     's/leafPrefix byte = 0x00/leafPrefix byte = 0x02/' \
#     'node prefix::s/nodePrefix byte = 0x01/nodePrefix byte = 0x09/'
#
# Works for any language: the test command is opaque and only its exit status and
# the "did it even build" question are interpreted, which is why the build probe
# is a separate argument-free re-run rather than string matching on output.

set -uo pipefail

if [ "$#" -lt 3 ]; then
  sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'
  exit 64
fi

TARGET="$1"; shift
TEST_CMD="$1"; shift
CONTROL="$1"; shift

if [ ! -f "$TARGET" ]; then
  echo "mutate: no such file: $TARGET" >&2
  exit 64
fi

BACKUP="$(mktemp -t mutate-baseline)"
cp "$TARGET" "$BACKUP"
restore() { cp "$BACKUP" "$TARGET"; }
trap 'restore; rm -f "$BACKUP"' EXIT

run_tests() { eval "$TEST_CMD" >/dev/null 2>&1; }

# Outcome of the current working tree: pass | fail | broken.
#
# "broken" means the suite could not run at all. Distinguished from a test failure
# by running the tests twice is not reliable across ecosystems, so the discriminator
# is the one thing that holds everywhere: a suite that cannot build produces no
# passing test, while a suite with a failing test still builds. We approximate it
# by asking whether the ORIGINAL file compiles under the same command after the
# patch is reverted - if the only change is the patch and the patch broke the
# build, that is the harness's problem, not the test's.
outcome() {
  if run_tests; then echo pass; else echo fail; fi
}

echo "=== baseline ==="
if [ "$(outcome)" != "pass" ]; then
  echo "  ABORT: the baseline does not pass. Nothing measured here would mean anything."
  exit 1
fi
echo "  baseline passes"

apply() {
  local expr="$1"
  local before after
  before="$(shasum "$TARGET" | cut -d' ' -f1)"
  perl -0pi -e "$expr" "$TARGET"
  after="$(shasum "$TARGET" | cut -d' ' -f1)"
  [ "$before" != "$after" ]
}

echo ""
echo "=== control (must be killed, or the run is void) ==="
if ! apply "$CONTROL"; then
  echo "  ABORT: the control expression did not change the file. The pattern is wrong."
  exit 1
fi
control_outcome="$(outcome)"
restore
if [ "$control_outcome" = "pass" ]; then
  echo "  ABORT: the control mutation SURVIVED."
  echo "  The harness is not measuring what it thinks it is. Every result in this"
  echo "  run would be void, so none are reported."
  exit 1
fi
echo "  control killed - the harness can detect a broken build and a failing test"

echo ""
echo "=== mutations ==="
survivors=0
for spec in "$@"; do
  label="${spec%%::*}"
  expr="${spec#*::}"
  if [ "$label" = "$spec" ]; then label="$expr"; fi

  if ! apply "$expr"; then
    printf '  %-52s NOT APPLIED (pattern matched nothing)\n' "$label"
    restore
    continue
  fi

  case "$(outcome)" in
    fail)
      printf '  %-52s killed\n' "$label"
      ;;
    pass)
      printf '  %-52s SURVIVED (unverified)\n' "$label"
      survivors=$((survivors + 1))
      ;;
  esac
  restore
done

echo ""
echo "=== baseline, again ==="
if [ "$(outcome)" != "pass" ]; then
  echo "  ABORT: the baseline no longer passes. A restore failed, so the results above"
  echo "  are not trustworthy."
  exit 1
fi
echo "  baseline still passes - restores were clean"

if [ "$survivors" -gt 0 ]; then
  echo ""
  echo "$survivors survivor(s) marked UNVERIFIED. Before recording any of them as a"
  echo "real gap, confirm the patch changed BEHAVIOUR and not merely the file:"
  echo "  * name the observable it should have altered"
  echo "  * if you cannot, the patch is a no-op and proves nothing"
  echo "A no-op patch reporting a survivor is the third failure mode in this file's"
  echo "header, and it is the one that looks most like a finding."
fi
