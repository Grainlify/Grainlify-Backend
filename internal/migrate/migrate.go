package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/migrations"
)

// NeedsMigration checks if migrations are needed by comparing the current database version
// with available migrations. Returns true if migrations are needed, false otherwise.
// This function queries the database directly to avoid acquiring locks.
func NeedsMigration(ctx context.Context, pool db.DBPool) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("db pool is nil")
	}

	slog.Info("checking if migrations are needed")
	
	// Query the schema_migrations table directly to avoid lock acquisition
	var currentVersion uint
	var dirty bool
	err := pool.QueryRow(ctx, `
		SELECT version, dirty 
		FROM schema_migrations 
		LIMIT 1
	`).Scan(&currentVersion, &dirty)
	
	if err != nil {
		// If table doesn't exist (relation does not exist) or no rows, assume first-time migration needed
		if err == pgx.ErrNoRows {
			slog.Info("no migration version found, assuming first-time migration needed")
			return true, nil
		}
		// Check if it's a "relation does not exist" error
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "does not exist") || strings.Contains(errStr, "relation") {
			slog.Info("schema_migrations table does not exist, assuming first-time migration needed")
			return true, nil
		}
		// Other errors - log and assume migrations needed to be safe
		slog.Warn("could not query schema_migrations table, assuming migrations needed", "error", err)
		return true, nil
	}

	if dirty {
		// Database is in dirty state - migrations are needed to fix it
		slog.Warn("database is in dirty state, migrations needed", "version", currentVersion)
		return true, nil
	}

	// Get the latest available migration version from source files
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		slog.Warn("could not load migration files, assuming migrations needed", "error", err)
		return true, nil
	}
	// Verify checksums of already applied migrations
	if err := verifyChecksums(pool, src); err != nil {
		// Fail fast on checksum mismatch
		return false, fmt.Errorf("migration checksum verification failed: %w", err)
	}

	latestVersion, err := getLatestMigrationVersion(src)
	if err != nil {
		slog.Warn("could not determine latest migration version, assuming migrations needed", "error", err)
		return true, nil
	}

	needsMigration := currentVersion < latestVersion
	if needsMigration {
		slog.Info("migrations needed",
			"current_version", currentVersion,
			"latest_version", latestVersion,
		)
	} else {
		slog.Info("migrations up to date",
			"current_version", currentVersion,
			"latest_version", latestVersion,
		)
	}

	return needsMigration, nil
}

// getLatestMigrationVersion extracts the highest version number from migration files
func getLatestMigrationVersion(src source.Driver) (uint, error) {
	firstVersion, err := src.First()
	if err != nil {
		// If no migrations exist, return 0
		return 0, fmt.Errorf("get first migration: %w", err)
	}

	var latestVersion uint = firstVersion
	currentVersion := firstVersion
	maxIterations := 1000 // Safety limit to prevent infinite loops
	iterations := 0

	for iterations < maxIterations {
		nextVersion, err := src.Next(currentVersion)
		if err != nil {
			// No more migrations - this is expected
			break
		}
		if nextVersion > latestVersion {
			latestVersion = nextVersion
		}
		currentVersion = nextVersion
		iterations++
	}

	if iterations >= maxIterations {
		return 0, fmt.Errorf("too many migrations, possible infinite loop")
	}

	return latestVersion, nil
}

func Up(ctx context.Context, pool db.DBPool, allowIrreversible bool) error {
	if pool == nil {
		return fmt.Errorf("db pool is nil")
	}

	slog.Info("loading embedded migration files")
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		slog.Error("failed to load embedded migrations",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	slog.Info("embedded migrations loaded")

	slog.Info("opening database connection for migrations")
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer sqlDB.Close()

	// Add random jitter (0-2 seconds) to avoid thundering herd problem
	// This helps when multiple instances start simultaneously
	jitter := time.Duration(rand.Intn(2000)) * time.Millisecond
	if jitter > 0 {
		slog.Info("adding random jitter before migration", "jitter_ms", jitter.Milliseconds())
		time.Sleep(jitter)
	}

	// Retry driver creation with simple fixed delay
	// The driver creation itself can fail if another instance is holding the lock
	maxDriverRetries := 10
	var db database.Driver
	for driverAttempt := 1; driverAttempt <= maxDriverRetries; driverAttempt++ {
		if driverAttempt > 1 {
			// Simple fixed delay: 500ms between attempts
			// No exponential backoff, no timeouts
			slog.Info("retrying postgres driver creation",
				"attempt", driverAttempt,
				"max_retries", maxDriverRetries,
			)
			time.Sleep(500 * time.Millisecond)
		}

		slog.Info("creating postgres migration driver", "attempt", driverAttempt)
		// Configure postgres driver - no lock_timeout set, let PostgreSQL handle it
		db, err = postgres.WithInstance(sqlDB, &postgres.Config{
			MigrationsTable:       "schema_migrations",
			DatabaseName:          "",
			SchemaName:            "",
			StatementTimeout:      0,
			MultiStatementEnabled: false,
		})
		if err == nil {
			break
		}

		// Check if it's a lock error (timeout or can't acquire)
		errStr := err.Error()
		isLockError := contains(errStr, "timeout") ||
			contains(errStr, "lock") ||
			contains(errStr, "can't acquire") ||
			contains(errStr, "55P03")

		if driverAttempt < maxDriverRetries && isLockError {
			slog.Info("postgres driver creation failed due to lock, will retry",
				"attempt", driverAttempt,
				"error", err,
			)
			continue
		}

		// For other errors or final attempt, return immediately
		slog.Error("failed to create postgres migration driver",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
			"attempt", driverAttempt,
		)
		return fmt.Errorf("create postgres migration driver: %w", err)
	}

	slog.Info("creating migrator instance")
	m, err := migrate.NewWithInstance("iofs", src, "postgres", db)
	if err != nil {
		slog.Error("failed to create migrator",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() {
		slog.Info("closing migrator")
		_, _ = m.Close()
	}()

	// Check current version before migrating
	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		slog.Warn("could not get current migration version",
			"error", err,
		)
	} else {
		slog.Info("current migration version",
			"version", version,
			"dirty", dirty,
		)
	}
	// Store the starting version for later checksum updates
	oldVersion := version

	// Collect pending migration versions for logging and irreversible checks.
	pendingVersions, err := collectPendingVersions(src, version, err)
	if err != nil {
		slog.Warn("could not enumerate pending migrations", "error", err)
	} else if len(pendingVersions) > 0 {
		slog.Info("pending migrations to apply", "versions", pendingVersions)
		if !allowIrreversible {
			for _, pv := range pendingVersions {
				irr, irrErr := isIrreversible(pv)
				if irrErr != nil {
					return fmt.Errorf("check migration %d irreversible status: %w", pv, irrErr)
				}
				if irr {
					return fmt.Errorf(
						"migration %d is marked irreversible; use --allow-irreversible or set MIGRATE_ALLOW_IRREVERSIBLE=1",
						pv,
					)
				}
			}
		}
	}

	// migrate.Up() is not context-aware; we still accept ctx for future evolutions.
	_ = ctx

	slog.Info("running database migrations")
	
	// Try to run migrations with simple retry logic
	// Use fixed short delays instead of exponential backoff
	maxRetries := 20
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			// Simple fixed delay: 500ms between attempts
			// No exponential backoff, no timeouts
			slog.Info("retrying migration after lock error",
				"attempt", attempt,
				"max_retries", maxRetries,
			)
			time.Sleep(500 * time.Millisecond)
		}
		
		err := m.Up()
		if err == nil || err == migrate.ErrNoChange {
			lastErr = err
			break
		}
		
		// Check if it's a lock error (timeout or can't acquire)
		errStr := err.Error()
		isLockError := contains(errStr, "timeout") || 
			contains(errStr, "lock") || 
			contains(errStr, "can't acquire") ||
			contains(errStr, "55P03")
		
		if attempt < maxRetries && isLockError {
			slog.Info("migration lock error, will retry",
				"attempt", attempt,
				"max_retries", maxRetries,
				"error", err,
			)
			lastErr = err
			continue
		}
		
		// For other errors or final attempt, return immediately
		lastErr = err
		break
	}
	
	if lastErr != nil && lastErr != migrate.ErrNoChange {
		slog.Error("migration failed after retries",
			"error", lastErr,
			"error_type", fmt.Sprintf("%T", lastErr),
		)
		return lastErr
	}
	
	err = lastErr

	if err == migrate.ErrNoChange {
		slog.Info("migrations up to date, no changes needed")
	} else {
		// Get version after migration
		newVersion, _, verErr := m.Version()
		if verErr == nil {
			slog.Info("migrations completed successfully",
				"new_version", newVersion,
			)
			// Update checksum records for any new migrations applied
			for v := oldVersion + 1; v <= newVersion; v++ {
				chk, err := migrations.ComputeChecksum(v)
				if err != nil {
					slog.Error("failed to compute checksum", "version", v, "error", err)
					continue
				}
				_, err = pool.Exec(context.Background(), `INSERT INTO migration_checksums (version, checksum) VALUES ($1, $2) ON CONFLICT (version) DO UPDATE SET checksum = EXCLUDED.checksum`, v, chk)
				if err != nil {
					slog.Error("failed to store checksum", "version", v, "error", err)
				}
			}
		} else {
			slog.Info("migrations completed successfully")
		}
	}

	return nil
}

// Down rolls back all applied migrations using the embedded migration files.
// It is the inverse of Up and is intended for use in tests and incident recovery.
func Down(ctx context.Context, pool db.DBPool) error {
	m, closer, err := newMigrator(pool)
	if err != nil {
		return err
	}
	defer closer()
	_ = ctx
	if err := m.Down(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Steps runs n migration steps. Positive n migrates up; negative n migrates down.
// It wraps golang-migrate's Steps using the same embedded iofs source.
func Steps(ctx context.Context, pool db.DBPool, n int) error {
	m, closer, err := newMigrator(pool)
	if err != nil {
		return err
	}
	defer closer()
	_ = ctx
	if err := m.Steps(n); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate steps(%d): %w", n, err)
	}
	return nil
}

// newMigrator creates a golang-migrate instance backed by the embedded migrations FS.
// The caller must invoke the returned closer to release resources.
func newMigrator(pool db.DBPool) (*migrate.Migrate, func(), error) {
	if pool == nil {
		return nil, nil, fmt.Errorf("db pool is nil")
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	dbDriver, err := postgres.WithInstance(sqlDB, &postgres.Config{
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("create postgres driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", dbDriver)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("create migrator: %w", err)
	}
	closer := func() {
		_, _ = m.Close()
		_ = sqlDB.Close()
	}
	return m, closer, nil
}

// collectPendingVersions enumerates migration versions that have not yet been applied.
// versionErr is the error returned by m.Version() — may be migrate.ErrNilVersion.
func collectPendingVersions(src source.Driver, currentVersion uint, versionErr error) ([]uint, error) {
	var pending []uint

	if versionErr == migrate.ErrNilVersion {
		// No migration has been applied yet. src.Next(v) requires v to be an
		// existing, known migration version (it looks up that version's own
		// file), so there's no sentinel "before the first migration" value we
		// can pass in — seed the list with First() directly and walk forward
		// from there with Next() instead.
		first, err := src.First()
		if err != nil {
			return nil, fmt.Errorf("get first migration: %w", err)
		}
		pending = append(pending, first)

		cur := first
		for {
			nextV, err := src.Next(cur)
			if err != nil {
				break
			}
			pending = append(pending, nextV)
			cur = nextV
		}
		return pending, nil
	} else if versionErr != nil {
		return nil, nil // unknown state — skip enumeration
	}

	cur := currentVersion
	for {
		nextV, err := src.Next(cur)
		if err != nil {
			break
		}
		pending = append(pending, nextV)
		cur = nextV
	}
	return pending, nil
}

// irreversibleVersions scans the embedded FS for *.irreversible marker files
// and returns a set of migration version numbers that are marked irreversible.
func irreversibleVersions() (map[uint]bool, error) {
	entries, err := fs.Glob(migrations.FS, "*.irreversible")
	if err != nil {
		return nil, fmt.Errorf("glob irreversible markers: %w", err)
	}
	versions := make(map[uint]bool, len(entries))
	for _, entry := range entries {
		parts := strings.SplitN(entry, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		versions[uint(v)] = true
	}
	return versions, nil
}

// isIrreversible reports whether the given migration version is marked irreversible.
func isIrreversible(version uint) (bool, error) {
	versions, err := irreversibleVersions()
	if err != nil {
		return false, err
	}
	return versions[version], nil
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}


// verifyChecksums ensures that applied migrations have matching checksums.
// It creates the migration_checksums table if it does not exist.
// For each applied migration version, it computes the checksum and compares
// it with the stored value, inserting a new record if absent.
// Returns an error if any checksum mismatches.
func verifyChecksums(pool db.DBPool, src source.Driver) error {
    // Ensure checksum table exists.
    _, err := pool.Exec(context.Background(), `
        CREATE TABLE IF NOT EXISTS migration_checksums (
            version bigint PRIMARY KEY,
            checksum text NOT NULL
        )
    `)
    if err != nil {
        return fmt.Errorf("create migration_checksums table: %w", err)
    }

    // Get applied migration versions (non‑dirty).
    rows, err := pool.Query(context.Background(), `
        SELECT version FROM schema_migrations WHERE dirty = false
    `)
    if err != nil {
        return fmt.Errorf("query applied migrations: %w", err)
    }
    defer rows.Close()

    for rows.Next() {
        var version uint
        if err := rows.Scan(&version); err != nil {
            return fmt.Errorf("scan version: %w", err)
        }
        // Compute current checksum.
        chk, err := migrations.ComputeChecksum(version)
        if err != nil {
            return fmt.Errorf("compute checksum for version %d: %w", version, err)
        }
        var stored string
        err = pool.QueryRow(context.Background(), `SELECT checksum FROM migration_checksums WHERE version = $1`, version).Scan(&stored)
        if err != nil {
            // No stored checksum – insert it.
            _, err = pool.Exec(context.Background(), `INSERT INTO migration_checksums (version, checksum) VALUES ($1, $2)`, version, chk)
            if err != nil {
                return fmt.Errorf("store checksum for version %d: %w", version, err)
            }
            continue
        }
        if stored != chk {
            return fmt.Errorf("checksum mismatch for migration %d: expected %s, got %s", version, stored, chk)
        }
    }
    if rows.Err() != nil {
        return fmt.Errorf("iteration error: %w", rows.Err())
    }
    return nil
}

