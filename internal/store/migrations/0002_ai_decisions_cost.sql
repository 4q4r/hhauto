-- 0002_ai_decisions_cost.sql — expand-only: per-decision cost tracking
-- (Wave 2 review / ruling R7).
-- ADD COLUMN with NOT NULL requires a non-NULL default per SQLite docs
-- (https://www.sqlite.org/lang_altertable.html); pre-existing rows are
-- backfilled with it.
ALTER TABLE ai_decisions ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0;
