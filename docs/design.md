# Hirewire: design

Hirewire watches company job boards and alerts users on Telegram within minutes of a matching job going live.

## Goals (v0 → v2)
- Alert a user about a matching new job **within 5 minutes** of it appearing (p95).
- **Never** send the same alert twice, and never lose one, even across crashes and restarts.
- Crawl only public ATS APIs (Greenhouse, Lever, Ashby) and company careers APIs. No LinkedIn/Naukri scraping.

## Architecture

```
                        ┌──────────────── one Go binary (hirewire) ────────────────┐
 Greenhouse / Lever ◀───┤ 1. Scheduler ──▶ 2. Crawler workers (goroutine pool)     │
 Ashby APIs             │    picks companies       fetch → diff against DB         │
                        │    that are due              ▼ one DB transaction:       │
                        │                   insert new jobs + matching             │
                        │                   notifications ("outbox")               │
 Telegram users ◀───────┤ 4. Notifier workers ◀────────┘  rate-limited, retries     │
          └────────────▶│ 3. Telegram bot: /start /subscribe /filters /stop        │
                        └──────────────────────────┬───────────────────────────────┘
                                                   ▼
                                              PostgreSQL
```

1. **Scheduler** claims due companies (`next_poll_at <= now()`) with `SELECT … FOR UPDATE SKIP LOCKED`, so no company is crawled twice at once.
2. **Crawler** fetches a board and inserts jobs with `ON CONFLICT DO NOTHING RETURNING id`: only truly new rows come back. In the same transaction it matches them against subscriptions and inserts `notifications` rows.
3. **Bot** (Telegram long polling) manages users and their filters.
4. **Notifier** sends pending notifications under Telegram's rate limit, retrying with backoff.

## Decisions

| Decision | Choice | Why | Revisit when |
|---|---|---|---|
| Service shape | Modular monolith: one binary, packages talk only through DB tables | One thing to deploy and debug; any component can be split out later | One component needs to scale independently |
| Queue | Postgres `notifications` table (outbox) | New jobs and their alerts commit atomically, so nothing is lost; no extra infrastructure | Sustained throughput Postgres can't absorb |
| Bot input | Telegram long polling | No public HTTPS endpoint needed; runs on a laptop | Moving to multiple bot instances |
| v0 filters | Include/exclude keywords + locations | Covers most needs; simple matching | Users ask for company/seniority filters |

## Correctness rules
- **No duplicate alerts:** `UNIQUE (user_id, job_id)` on `notifications`.
- **No flood when a company is added:** the first crawl records a baseline and alerts nobody (`companies.baseline_done`).
- **A broken board is loud, not silent:** non-200 responses are errors and bump `consecutive_failures`.

## Data model
See `internal/store/schema.sql`.
