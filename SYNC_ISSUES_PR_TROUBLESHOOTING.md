# Sync Issues / PR Troubleshooting – Index Rationale (migration 000029)

## Problem

Two hot query paths cause sequential scans as data grows:

1. **sync_jobs claim query** (`internal/syncjobs/worker.go`, runs every second per worker):
   ```sql
   SELECT id, project_id, job_type
   FROM sync_jobs
   WHERE status = 'pending' AND run_at <= now()
   ORDER BY run_at ASC
   FOR UPDATE SKIP LOCKED LIMIT 1
   ```
   Migration 000003 created `idx_sync_jobs_pending ON sync_jobs(status, run_at)` –
   a full-table btree that grows with every completed/failed row even though the
   query only ever touches `pending` rows.

2. **Leaderboard / profile aggregation** (`internal/handlers/leaderboard.go`,
   `internal/handlers/user_profile.go`):
   ```sql
   WHERE LOWER(i.author_login) = LOWER(ac.login)   -- github_issues
   WHERE LOWER(pr.author_login) = LOWER(ac.login)  -- github_pull_requests
   LEFT JOIN github_accounts ga ON LOWER(ga.login) = LOWER(ac.login)
   ```
   The plain btree indexes from migration 000014 store the original mixed-case
   value; a predicate on `LOWER(column)` bypasses them entirely.

## Migration 000029 indexes

| Index | Table | Definition | Replaces |
|---|---|---|---|
| `idx_sync_jobs_claim` | `sync_jobs` | `(run_at ASC) WHERE status = 'pending'` | `idx_sync_jobs_pending` (dropped) |
| `idx_github_issues_author_login_lower` | `github_issues` | `(LOWER(author_login)) WHERE author_login IS NOT NULL` | — additive |
| `idx_github_prs_author_login_lower` | `github_pull_requests` | `(LOWER(author_login)) WHERE author_login IS NOT NULL` | — additive |
| `idx_github_accounts_login_lower` | `github_accounts` | `(LOWER(login)) WHERE login IS NOT NULL` | — additive |

The partial index on `sync_jobs` contains only pending rows, so it stays small
regardless of how many completed/failed rows accumulate. The btree is already
ordered by `run_at`, eliminating the sort step in `ORDER BY run_at ASC LIMIT 1`.

## Reconciliation with 000014 / 000015

The existing case-sensitive `idx_github_issues_author_login` /
`idx_github_prs_author_login` / `idx_github_accounts_login` from 000014 are
**kept** – the profile handler uses exact-case `WHERE author_login = $1` and
benefits from them. The new `_lower` functional indexes are additive and serve
the leaderboard's case-folded path only.

No indexes from 000015 (date-range indexes) overlap.

## EXPLAIN ANALYZE (expected after migration)

```
-- Claim query
Index Scan using idx_sync_jobs_claim on sync_jobs
  Index Cond: (run_at <= now())   -- partial predicate already filters status

-- Leaderboard sub-select
Index Scan using idx_github_issues_author_login_lower on github_issues
  Index Cond: (lower(author_login) = lower(...))
```

## Rollback

```bash
go run ./cmd/migrate -steps -1
```

The down migration drops only the four indexes above and restores
`idx_sync_jobs_pending`. No other indexes are touched.

## Security

Index-only DDL change. No row visibility, permissions, or RLS policies are
altered. The partial and functional indexes do not expose data beyond what the
querying role could already read.
