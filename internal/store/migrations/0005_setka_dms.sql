-- 0005_setka_dms.sql — T61 Setka DM ledger.
-- One row per (account, person, vacancy) attempt: the idempotency record of
-- the setka feature (a repeat run never messages the same person twice for
-- one vacancy), the per-day / per-company / per-person limit counters and
-- the approve-mode hand-off to the review queue (review_id). The UNIQUE
-- index is the DB-level backstop of the exists-check.
-- expand-only migration.

CREATE TABLE IF NOT EXISTS setka_dms (
    id                    INTEGER PRIMARY KEY,
    account_id            TEXT NOT NULL,
    application_dedup_key TEXT NOT NULL,
    vacancy_id            INTEGER,
    employer_id           INTEGER,
    person_id             TEXT NOT NULL,
    person_name           TEXT,
    company               TEXT,
    mode                  TEXT NOT NULL CHECK (mode IN ('auto','approve')),
    status                TEXT NOT NULL CHECK (status IN ('queued_review','sent','skipped')),
    text                  TEXT,
    review_id             INTEGER,
    reason                TEXT,
    created_at            TEXT NOT NULL,
    sent_at               TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_setka_dms_account_person_vacancy
    ON setka_dms (account_id, person_id, vacancy_id);

CREATE INDEX IF NOT EXISTS idx_setka_dms_account_status_created
    ON setka_dms (account_id, status, created_at);
