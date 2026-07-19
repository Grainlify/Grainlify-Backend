# Database Migrations

Migrations are embedded via `//go:embed` in `migrations/migrations.go` and applied with
[golang-migrate/migrate](https://github.com/golang-migrate/migrate).

## File naming

Each migration consists of two SQL files inside `migrations/`:

```
NNNNNN_description.up.sql   — forward migration
NNNNNN_description.down.sql — rollback
```

The version number (`NNNNNN`) must be zero-padded to six digits, must increase
sequentially, and must be unique.

## Irreversible migrations

A migration that drops a column, deletes data, or otherwise cannot be safely
reversed should be marked **irreversible**. This is done by creating an empty
marker file alongside the SQL files:

```
NNNNNN_description.irreversible
```

For example, `000007_remove_chain_from_projects.irreversible` marks migration 7
as irreversible.

### Guard behaviour

| Environment | Irreversible marker present | Behaviour |
|---|---|---|
| dev | yes | migration runs (no flag needed) |
| dev | no | migration runs normally |
| non-dev | yes | **blocked** unless `--allow-irreversible` or `MIGRATE_ALLOW_IRREVERSIBLE=1` |
| non-dev | no | migration runs normally |

### Opt-in flags

- **CLI flag:** `go run ./cmd/migrate --allow-irreversible`
- **Environment variable:** `MIGRATE_ALLOW_IRREVERSIBLE=1`

When used via the API server (`cmd/api/main.go`), only the environment variable
is honoured.

### Logging

Before any migrations are applied, the runner logs all pending version numbers:

```
INFO pending migrations to apply versions=[7 8 9 10]
```

If an irreversible migration is blocked, a clear error is returned:

```
migration 7 is marked irreversible; use --allow-irreversible or set MIGRATE_ALLOW_IRREVERSIBLE=1
```

## Adding a new migration

1. Create `migrations/NNNNNN_description.up.sql` with the forward SQL.
2. Create `migrations/NNNNNN_description.down.sql` with the rollback SQL.
3. If the migration is **destructive or cannot be reversed**, also create an empty
   `migrations/NNNNNN_description.irreversible` marker file.
4. Run `go run ./cmd/migrate` to apply.
