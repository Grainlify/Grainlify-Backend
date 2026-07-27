# Worker Architecture

## Overview

Grainlify runs two separate processes from the same binary:

| Role | Binary | Entry point | Required env |
|------|--------|-------------|-------------|
| API server | `./api` | `cmd/api` | `DB_URL`, `JWT_SECRET`, GitHub credentials |
| Background worker | `./worker` | `cmd/worker` | `DB_URL`, `NATS_URL` |

The processes are **distinct binaries** built from the same module.  
A single container image can run either role by changing the start command —
there is no `APP_ROLE` dispatch inside a single binary. `APP_ROLE=api|worker`
in `.env.example` is purely documentation for operators and Railway service
configuration.

---

## What the worker does

```
NATS (grainlify.github.webhook.received)
        │  queue-group: grainlify-workers
        ▼
GitHubWebhookConsumer
  └─ internal/worker.GitHubWebhookConsumer.Subscribe
       └─ ingest.GitHubWebhookIngestor.Ingest → PostgreSQL

PostgreSQL (sync_jobs table)
        │  FOR UPDATE SKIP LOCKED
        ▼
syncjobs.Worker.Run
  └─ processOne every 1 s
       ├─ sync_issues → github_issues
       └─ sync_prs    → github_pull_requests
```

### GitHub webhook consumer

- Subscribes to subject `grainlify.github.webhook.received` with queue group
  `grainlify-workers`, so multiple worker replicas share the message load
  without duplicate processing.
- Message handling is synchronous within the subscription callback — slow
  ingest naturally applies back-pressure to NATS.

### Syncjobs worker

- Polls `sync_jobs` every second using `FOR UPDATE SKIP LOCKED` so multiple
  replicas never duplicate a job.
- Rate-limited to ~4 GitHub API requests per second (burst 2).
- Job types: `sync_issues`, `sync_prs`.

---

## Graceful shutdown

```
SIGINT / SIGTERM
      │
      ▼
signal.NotifyContext cancels root ctx
      │
      ├─ GitHubWebhookConsumer: ctx.Done() → Unsubscribe()
      └─ syncjobs.Worker.Run: ctx.Done() → return ctx.Err()

sync.WaitGroup.Wait() — up to 10 s
      │
      ├─ NATS Drain + Close
      └─ pgxpool Close
```

In-flight webhook and sync-job processing is allowed to complete within the
10-second window before the process exits.

---

## Fail-fast rules

| Condition | Behaviour |
|-----------|-----------|
| `DB_URL` empty, any env | `os.Exit(1)` — worker is useless without DB |
| `NATS_URL` empty, any env | `os.Exit(1)` — worker is useless without NATS |
| DB connect fails | `os.Exit(1)` |
| NATS connect fails | `os.Exit(1)` |
| NATS subscribe fails | `os.Exit(1)` |

The worker never starts serving traffic in a degraded state.

---

## Security

- Secrets (`DB_URL`, `NATS_URL`, tokens) are read from environment variables,
  never from files or flags.
- Passwords in connection URLs are masked before logging (handled by
  `internal/db` and `internal/bus/natsbus`).
- Secret values are never printed in log output.
- The worker reuses the same config and connection packages as `cmd/api`,
  so any security hardening applies to both.

---

## Building and running

```bash
# Build
make build-worker          # produces ./worker

# Run (requires DB_URL and NATS_URL in environment or .env)
make run-worker            # go run ./cmd/worker

# Or run the binary directly
DB_URL=postgresql://... NATS_URL=nats://... ./worker
```

---

## Deployment on Railway

See [RAILWAY_DEPLOYMENT.md](RAILWAY_DEPLOYMENT.md#worker-service) for how to
create a second Railway service that runs `./worker` from the same repo.
