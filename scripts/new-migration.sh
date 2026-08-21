#!/usr/bin/env bash
# Create an up/down migration pair numbered by timestamp.
#
#   scripts/new-migration.sh add_contributor_public_keys
#
# Why a timestamp and not the next integer: two branches created a minute apart
# pick the same next integer, and that collision is only detectable in some of
# its windows - the across-branches CI check needs both branches pushed, and the
# above-main check needs one of them merged. Neither sees two unpushed branches.
# A timestamp cannot be produced twice, so the class does not arise.
#
# Digits only. golang-migrate parses the version with ^([0-9]+)_ and
# ParseUint(.., 10, 64); a separator like 20260821T143702 makes the file
# invisible to the driver rather than failing loudly.
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <snake_case_name>" >&2
  exit 2
fi

name="$1"
if ! printf '%s' "$name" | grep -qE '^[a-z0-9_]+$'; then
  echo "error: name must be snake_case: [a-z0-9_]+" >&2
  exit 2
fi

version=$(date -u +%Y%m%d%H%M%S)
dir="$(cd "$(dirname "$0")/.." && pwd)/migrations"

up="$dir/${version}_${name}.up.sql"
down="$dir/${version}_${name}.down.sql"

for f in "$up" "$down"; do
  if [ -e "$f" ]; then
    echo "error: $f already exists" >&2
    exit 1
  fi
done

printf -- '-- %s\n--\n-- Why this change is needed, and what breaks without it.\n\n' "$name" > "$up"
printf -- '-- Reverse of %s.\n--\n-- Rolling back LOCALLY means dropping the database, not running this by\n-- hand: hand-editing schema_migrations leaves the recorded version\n-- disagreeing with the files. See docs/RUNBOOK-ci.md.\n\n' "$version" > "$down"

echo "$up"
echo "$down"
