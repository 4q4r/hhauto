-- 0004_resume_improve_diff.sql — T29 per-field improve audit.
-- The diff (было/стало per field, JSON) of every AI improvement applied to
-- a clone is stored on its lineage row: one audit artifact per clone,
-- written after the verify gate passes (or parked for review in dry-run).
-- NULL = the clone was never improved (improve disabled or failed before
-- any field landed).

ALTER TABLE resume_clones ADD COLUMN improve_diff TEXT;
