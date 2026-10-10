-- 0007_rejections.sql — T123 rejection loop storage.
-- One analyzed rejection per (account, vacancy): a re-analysis replaces the
-- row in place (UNIQUE backstop), so repeated `rejections analyze` runs are
-- idempotent and never burn tokens twice. evidence_based marks causes
-- confirmed by the employer's own words vs the model's guess (guesses stay
-- stored, marked, and are reported without shares). MVP is report-only:
-- nothing here feeds an automatic resume edit.
-- expand-only migration.

CREATE TABLE IF NOT EXISTS rejections (
    id              INTEGER PRIMARY KEY,
    account_id      TEXT NOT NULL,
    vacancy_id      INTEGER NOT NULL,
    vacancy_name    TEXT NOT NULL DEFAULT '',
    employer_name   TEXT NOT NULL DEFAULT '',
    negotiation_id  TEXT NOT NULL DEFAULT '',
    rejected_at     TEXT NOT NULL,
    root_cause      TEXT NOT NULL,
    evidence_based  INTEGER NOT NULL DEFAULT 0,
    improved_letter TEXT NOT NULL DEFAULT '',
    missing_skills  TEXT NOT NULL DEFAULT '[]',
    takeaway        TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_rejections_key
    ON rejections (account_id, vacancy_id);

CREATE INDEX IF NOT EXISTS idx_rejections_account
    ON rejections (account_id, rejected_at);
