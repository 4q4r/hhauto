-- 0001_init.sql — baseline schema for hhauto (expand-only migrations).
-- Timestamps are RFC3339 UTC TEXT. Migration version tracking uses
-- PRAGMA user_version; this file runs inside one transaction together
-- with the schema_migrations audit row and the user_version bump.
-- Names below are integration contract: other packages depend on them.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT NOT NULL PRIMARY KEY,
    display_name TEXT,
    hh_user_id   INTEGER,
    status       TEXT NOT NULL DEFAULT 'unknown'
                 CHECK (status IN ('ok','paused','logged_out','blocked','unknown')),
    last_ok_at   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS resumes (
    id           INTEGER PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    hh_resume_id TEXT NOT NULL,
    title        TEXT,
    status       TEXT,
    raw          TEXT,
    updated_at   TEXT,
    UNIQUE (account_id, hh_resume_id)
);

CREATE TABLE IF NOT EXISTS vacancies_seen (
    id            INTEGER PRIMARY KEY,
    account_id    TEXT NOT NULL,
    vacancy_id    INTEGER NOT NULL,
    source        TEXT,
    first_seen_at TEXT,
    meta          TEXT,
    UNIQUE (account_id, vacancy_id)
);

CREATE INDEX IF NOT EXISTS idx_vacancies_seen_account_source_first_seen
    ON vacancies_seen (account_id, source, first_seen_at);

CREATE TABLE IF NOT EXISTS applications (
    id             INTEGER PRIMARY KEY,
    account_id     TEXT NOT NULL,
    run_id         TEXT,
    hh_resume_id   TEXT,
    vacancy_id     INTEGER,
    employer_id    INTEGER,
    dedup_key      TEXT NOT NULL UNIQUE,
    status         TEXT NOT NULL
                   CHECK (status IN ('queued','dry_run','applied','unknown','failed',
                                     'skipped_blacklist','skipped_exclude','skipped_rating',
                                     'skipped_ai','skipped_cap')),
    letter         TEXT,
    fail_reason    TEXT,
    correlation_id TEXT,
    created_at     TEXT NOT NULL,
    applied_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_applications_account_run_status_applied
    ON applications (account_id, run_id, status, applied_at);

CREATE TABLE IF NOT EXISTS chats (
    id                   INTEGER PRIMARY KEY,
    account_id           TEXT NOT NULL,
    chatik_chat_id       TEXT NOT NULL,
    topic_id             TEXT,
    vacancy_id           INTEGER,
    employer_id          INTEGER,
    employer_name        TEXT,
    state                TEXT NOT NULL DEFAULT 'active'
                         CHECK (state IN ('active','rejected','left','escalated','warmup_due')),
    rejection_confidence REAL,
    last_message_at      TEXT,
    last_outgoing_at     TEXT,
    leave_count          INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT,
    UNIQUE (account_id, chatik_chat_id)
);

CREATE INDEX IF NOT EXISTS idx_chats_account_state_last_outgoing
    ON chats (account_id, state, last_outgoing_at);

CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY,
    chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL UNIQUE,
    direction  TEXT NOT NULL CHECK (direction IN ('in','out')),
    author     TEXT,
    text       TEXT,
    sent_at    TEXT,
    created_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_chat_sent
    ON messages (chat_id, sent_at);

CREATE TABLE IF NOT EXISTS contacts (
    id           INTEGER PRIMARY KEY,
    account_id   TEXT NOT NULL,
    employer_id  INTEGER NOT NULL,
    source       TEXT NOT NULL CHECK (source IN ('card','chat','email')),
    name         TEXT,
    role         TEXT,
    phones       TEXT,
    emails       TEXT,
    messengers   TEXT,
    raw          TEXT,
    collected_at TEXT,
    UNIQUE (account_id, employer_id, source)
);

CREATE TABLE IF NOT EXISTS tests (
    id           INTEGER PRIMARY KEY,
    account_id   TEXT NOT NULL,
    vacancy_id   INTEGER,
    kind         TEXT NOT NULL CHECK (kind IN ('hh_internal','external_form','unknown')),
    provider     TEXT,
    external_url TEXT,
    status       TEXT NOT NULL DEFAULT 'detected'
                 CHECK (status IN ('detected','in_progress','solved','escalated','failed','skipped')),
    answers      TEXT,
    result       TEXT,
    created_at   TEXT,
    finished_at  TEXT
);

CREATE TABLE IF NOT EXISTS interview_links (
    id          INTEGER PRIMARY KEY,
    account_id  TEXT NOT NULL,
    chat_id     INTEGER,
    url         TEXT NOT NULL,
    provider    TEXT,
    status      TEXT NOT NULL DEFAULT 'detected',
    detected_at TEXT,
    UNIQUE (account_id, url)
);

CREATE TABLE IF NOT EXISTS ping_queue (
    id           INTEGER PRIMARY KEY,
    account_id   TEXT NOT NULL,
    chat_id      INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    due_at       TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 2,
    state        TEXT NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending','sent','cancelled','exhausted')),
    created_at   TEXT,
    sent_at      TEXT,
    UNIQUE (chat_id, attempts)
);

CREATE TABLE IF NOT EXISTS ai_decisions (
    id           TEXT NOT NULL PRIMARY KEY,
    created_at   TEXT,
    account_id   TEXT,
    task         TEXT,
    entity_type  TEXT,
    entity_id    TEXT,
    run_id       TEXT,
    model        TEXT,
    prompt_ver   TEXT,
    input_digest TEXT,
    verdict      TEXT,
    score        REAL,
    confidence   REAL,
    needs_human  INTEGER,
    injection    INTEGER,
    latency_ms   INTEGER,
    tokens_in    INTEGER,
    tokens_out   INTEGER,
    cache_hit    INTEGER,
    outcome      TEXT NOT NULL DEFAULT 'pending'
);

CREATE INDEX IF NOT EXISTS idx_ai_decisions_task_entity_created
    ON ai_decisions (task, entity_type, entity_id, created_at);

CREATE TABLE IF NOT EXISTS schedules (
    id          TEXT NOT NULL PRIMARY KEY,
    account_id  TEXT,
    spec        TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at TEXT,
    next_run_at TEXT,
    updated_at  TEXT
);

CREATE TABLE IF NOT EXISTS feature_state (
    feature    TEXT NOT NULL,
    account_id TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT,
    updated_at TEXT,
    PRIMARY KEY (feature, account_id, key)
);

CREATE TABLE IF NOT EXISTS review_items (
    id          INTEGER PRIMARY KEY,
    account_id  TEXT NOT NULL,
    kind        TEXT,
    reason      TEXT,
    payload     TEXT,
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','resolved','ignored')),
    resolution  TEXT,
    created_at  TEXT,
    resolved_at TEXT
);

CREATE TABLE IF NOT EXISTS employer_ratings (
    employer_id   INTEGER PRIMARY KEY,
    score_all     REAL,
    score_recent  REAL,
    period_days   INTEGER,
    reviews_count INTEGER,
    recommend_pct INTEGER,
    fetched_at    TEXT
);

CREATE TABLE IF NOT EXISTS metrics (
    key   TEXT NOT NULL,
    day   TEXT NOT NULL,
    value INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key, day)
);

CREATE TABLE IF NOT EXISTS intents (
    intent_id  TEXT NOT NULL PRIMARY KEY,
    key        TEXT NOT NULL UNIQUE,
    state      TEXT NOT NULL CHECK (state IN ('pending','done','failed','abandoned')),
    payload    TEXT,
    created_at TEXT,
    updated_at TEXT
);

CREATE TABLE IF NOT EXISTS captcha_events (
    id          INTEGER PRIMARY KEY,
    account_id  TEXT,
    provider_id TEXT,
    kind        TEXT,
    ok          INTEGER,
    latency_ms  INTEGER,
    created_at  TEXT
);

CREATE TABLE IF NOT EXISTS webhook_outbox (
    id            INTEGER PRIMARY KEY,
    endpoint_id   TEXT,
    payload       TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    next_retry_at TEXT,
    delivered     INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT
);

CREATE TABLE IF NOT EXISTS profile_facts (
    account_id TEXT NOT NULL,
    question   TEXT NOT NULL,
    answer     TEXT,
    updated_at TEXT,
    PRIMARY KEY (account_id, question)
);
