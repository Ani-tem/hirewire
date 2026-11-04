# hirewire

Get a Telegram alert the moment a company you care about posts a job that matches you.

hirewire polls the public job-board APIs that companies use (Greenhouse, Lever, Ashby and Workday, covering 184 companies including CRED, Meesho, Paytm, Sarvam AI, Stripe, OpenAI, NVIDIA and Salesforce), works out which postings are new, and alerts every user whose filters match. It is written in Go and runs as a single binary backed by PostgreSQL.

> **Status:** in progress. Fetching, company seeding, new-job detection, the first-crawl baseline, scheduled concurrent crawling and keyword matching work. The outbox and Telegram delivery are next. See the [roadmap](#roadmap).

## How it works

```
 Scheduler ──► Crawler workers ──► Postgres ◄── Telegram bot (user commands)
  (due companies,   (fetch · diff · match)     │
   SKIP LOCKED)                                └──► Notifier ──► Telegram
                                                    (outbox, rate limit, retries)
```

The modules talk to each other only through Postgres. The full write-up is in [docs/design.md](docs/design.md). The main correctness choices:

- **New-job detection inside the database.** A whole board goes in as one batched `INSERT … ON CONFLICT DO NOTHING RETURNING id`. Only jobs never seen before come back, with no separate read-then-write race.
- **Idempotent writes.** Seeding and re-crawling are safe to repeat. Unique keys (`(ats, slug)`, `(company_id, external_id)`) make duplicates impossible, not merely unlikely.
- **No alert floods.** A company's first crawl is a silent baseline. Alerts start from the second crawl.
- **Exactly-one alert per user per job.** A transactional outbox (`notifications` table) with `UNIQUE(user_id, job_id)` means a crash or retry never double-sends or drops an alert.
- **Only public APIs.** No scraping of LinkedIn or Naukri.

## Run it locally

Requires Go 1.27+ and Docker.

```bash
docker compose up -d                # Postgres on localhost:5433
go test ./...                       # store tests need the database running
go run ./cmd/hirewire seed          # load data/companies.csv
go run ./cmd/hirewire crawl stripe  # fetch a board and report new jobs
TELEGRAM_BOT_TOKEN=... go run ./cmd/hirewire run   # crawl, answer the bot and send alerts until Ctrl-C
go run ./cmd/hirewire try -include "backend,sde" -exclude "senior,staff" -location "bengaluru,remote india"
                                    # which stored jobs would this subscription get?
```

```
Stripe: baseline saved, 745 jobs already on the board (no alerts)
Stripe: 745 jobs on the board, 0 new
```

Set `DATABASE_URL` to use a different Postgres.

## Layout

```
cmd/hirewire/       CLI entry point (run, seed, try, crawl, fetch)
internal/bot/       Telegram commands: /subscribe, /list, /unsubscribe, /stop
internal/crawler/   dispatcher + worker pool: claim due boards, crawl in parallel, record
internal/notifier/  drain the outbox: claim pending alerts, one message per chat, rate limit, retry
internal/fetcher/   one client per ATS (Greenhouse, Lever, Ashby, Workday), all returning the same Job type
internal/match/     keyword and location matching on whole words
internal/store/     all SQL; schema.sql is embedded into the binary
internal/telegram/  minimal Bot API client (long polling, sendMessage, 429 retry_after)
data/companies.csv  the boards we track
docs/design.md      high-level design and decisions
```

## Roadmap

- [x] Greenhouse fetcher
- [x] Postgres schema and idempotent company seeding
- [x] Batched save with new-job detection
- [x] Baseline on first crawl
- [x] Scheduler with `FOR UPDATE SKIP LOCKED`
- [x] Concurrent crawler worker pool
- [x] Lever and Ashby fetchers (more Indian companies)
- [x] Keyword and location matching
- [x] Transactional outbox: notifications queued in the same transaction as the jobs
- [x] Telegram bot: subscribe, list, unsubscribe, stop
- [x] Rate-limited notifier with retries
- [x] Workday fetcher (newest 100 per board, multi-location jobs resolved and cached)
- [ ] Deploy, then real users
