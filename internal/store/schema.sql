-- Applied on every startup, so every statement must be idempotent.
-- shortcut: no migration tool; switch to versioned migrations before the first destructive schema change.

CREATE TABLE IF NOT EXISTS companies (
    id                   BIGSERIAL PRIMARY KEY,
    name                 TEXT        NOT NULL,
    ats                  TEXT        NOT NULL,              -- checked by companies_ats_check below
    slug                 TEXT        NOT NULL,              -- board name on the ATS, e.g. "stripe"
    poll_interval        INTERVAL    NOT NULL DEFAULT '15 minutes',
    next_poll_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_success_at      TIMESTAMPTZ,
    consecutive_failures INT         NOT NULL DEFAULT 0,
    baseline_done        BOOLEAN     NOT NULL DEFAULT false, -- first crawl records jobs without alerting
    enabled              BOOLEAN     NOT NULL DEFAULT true,
    UNIQUE (ats, slug)
);
-- Kept outside CREATE TABLE so adding an ATS reaches databases that already exist.
ALTER TABLE companies DROP CONSTRAINT IF EXISTS companies_ats_check;
ALTER TABLE companies ADD CONSTRAINT companies_ats_check
    CHECK (ats IN ('greenhouse', 'lever', 'ashby', 'workday'));
CREATE INDEX IF NOT EXISTS companies_due_idx ON companies (next_poll_at) WHERE enabled;

CREATE TABLE IF NOT EXISTS jobs (
    id            BIGSERIAL PRIMARY KEY,
    company_id    BIGINT      NOT NULL REFERENCES companies (id),
    external_id   TEXT        NOT NULL,                     -- the ATS's own ID
    title         TEXT        NOT NULL,
    location      TEXT        NOT NULL DEFAULT '',
    url           TEXT        NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at    TIMESTAMPTZ,
    UNIQUE (company_id, external_id)                        -- identity of a job; makes inserts dedupe
);

CREATE TABLE IF NOT EXISTS users (
    id               BIGSERIAL PRIMARY KEY,
    telegram_chat_id BIGINT      NOT NULL UNIQUE,
    active           BOOLEAN     NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS subscriptions (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    include_keywords TEXT[]      NOT NULL DEFAULT '{}',     -- any must appear in the title
    exclude_keywords TEXT[]      NOT NULL DEFAULT '{}',     -- none may appear in the title
    locations        TEXT[]      NOT NULL DEFAULT '{}',     -- empty = anywhere
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscriptions_user_idx ON subscriptions (user_id);

-- The outbox: written in the same transaction as the jobs it alerts about.
CREATE TABLE IF NOT EXISTS notifications (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_id          BIGINT      NOT NULL REFERENCES jobs (id),
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, job_id)                                -- never alert the same user twice for a job
);
CREATE INDEX IF NOT EXISTS notifications_pending_idx ON notifications (next_attempt_at) WHERE status = 'pending';
