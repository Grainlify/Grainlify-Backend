package migrate_test

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/migrations"
)

// The 0000xx migration series is closed. New migrations must be timestamped.
//
// golang-migrate applies only the versions ABOVE the one in schema_migrations.
// This repository's numbering moved from a six-digit series to timestamps, and
// every database that exists is now far past the six-digit range - so a new
// 0000xx migration is applied to nothing. It does not fail, it does not warn,
// and `migrate up` reports success.
//
// This is not hypothetical. 000090 added hackathon_assignments.expiry_warned_at,
// deployed green, and produced this in production once a minute:
//
//	hackathon.WarnExpiring: ERROR: column a.expiry_warned_at does not exist
//
// The regression suite could not catch it: it recreates its database every run,
// so the whole series applies in order and everything is present. Only a
// database that already existed shows the difference - which is to say, only
// production.
//
// So the check is on the numbering itself. The count below is the closed
// series. Adding to it fails here; adding a timestamp does not.
func TestLegacyMigrationSeriesIsClosed(t *testing.T) {
	legacy := regexp.MustCompile(`^(\d{6})_.*\.up\.sql$`)
	timestamped := regexp.MustCompile(`^(\d{14})_.*\.up\.sql$`)

	var legacyCount, timestampedCount int
	var highestLegacy, lowestTimestamp uint64 = 0, ^uint64(0)
	var unrecognised []string

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		switch {
		case legacy.MatchString(name):
			legacyCount++
			v, _ := strconv.ParseUint(legacy.FindStringSubmatch(name)[1], 10, 64)
			if v > highestLegacy {
				highestLegacy = v
			}
		case timestamped.MatchString(name):
			timestampedCount++
			v, _ := strconv.ParseUint(timestamped.FindStringSubmatch(name)[1], 10, 64)
			if v < lowestTimestamp {
				lowestTimestamp = v
			}
		default:
			unrecognised = append(unrecognised, name)
		}
	}

	if len(unrecognised) > 0 {
		t.Errorf("migration files whose version this check cannot read: %s", strings.Join(unrecognised, ", "))
	}
	if timestampedCount == 0 || legacyCount == 0 {
		t.Fatalf("read %d legacy and %d timestamped migrations; the scan is not seeing the files",
			legacyCount, timestampedCount)
	}

	// Every six-digit version is below every timestamp, which is what makes
	// the series unreachable rather than merely old.
	if highestLegacy >= lowestTimestamp {
		t.Fatalf("the two series overlap (highest legacy %d, lowest timestamp %d); this check's premise is wrong",
			highestLegacy, lowestTimestamp)
	}

	// The closed series. If you are here because you added a migration: give
	// it a timestamp version (date +%Y%m%d%H%M%S) instead of the next number.
	// A six-digit migration will not run on any database that already exists.
	const closedSeriesSize = 89
	if legacyCount != closedSeriesSize {
		t.Errorf("there are %d six-digit migrations, expected %d.\n"+
			"If one was added: rename it to a timestamp version (date +%%Y%%m%%d%%H%%M%%S). "+
			"Versions below %d are already behind every live database and will never run - "+
			"deploys go green and the column is simply missing.",
			legacyCount, closedSeriesSize, lowestTimestamp)
	}
}
