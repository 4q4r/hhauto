-- 0003_resume_clones.sql — T26 autoclone lineage (expand-only).
-- hh.ru does not expose the clone parent (live clones carry
-- status="not_finished" and no parentResumeId), so hhauto tracks the
-- lineage locally: one row per created clone, keyed by the hh resume hash.
-- Timestamps are RFC3339 UTC TEXT (0001 convention).

CREATE TABLE IF NOT EXISTS resume_clones (
    clone_hash  TEXT NOT NULL PRIMARY KEY,
    parent_hash TEXT NOT NULL,
    depth       INTEGER NOT NULL,
    account_id  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_resume_clones_account_parent
    ON resume_clones (account_id, parent_hash);
