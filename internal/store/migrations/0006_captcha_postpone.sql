-- 0006_captcha_postpone.sql — T89 captcha postpone registry.
-- One row per (account, resume, vacancy) parked by a captcha challenge:
-- retry_at schedules the re-attack (the daemon wake rides the queue task at
-- the same moment), retry_count bounds the cycle (max_retries exhausted →
-- captcha_exhausted). The postpone is NOT an applications row: it must
-- never block a future apply (dedup reads applied only) and the applications
-- CHECK vocabulary stays untouched.
-- expand-only migration.

CREATE TABLE IF NOT EXISTS captcha_postponed (
    id           INTEGER PRIMARY KEY,
    account_id   TEXT NOT NULL,
    resume_hash  TEXT NOT NULL,
    vacancy_id   INTEGER NOT NULL,
    run_id       TEXT,
    retry_at     TEXT NOT NULL,
    retry_count  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_captcha_postponed_key
    ON captcha_postponed (account_id, resume_hash, vacancy_id);

CREATE INDEX IF NOT EXISTS idx_captcha_postponed_due
    ON captcha_postponed (account_id, resume_hash, retry_at);
