package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// AIDecisionsAdd records an AI decision; d.ID receives a fresh ULID when
// empty, d.CreatedAt is filled when zero, empty Outcome falls back to
// 'pending'.
func (db *DB) AIDecisionsAdd(ctx context.Context, d *AIDecision) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	if d.ID == "" {
		d.ID = ulid.Make().String()
	}
	if d.Outcome == "" {
		d.Outcome = "pending"
	}
	createdAt := d.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
		d.CreatedAt = createdAt
	}

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO ai_decisions (id, created_at, account_id, task, entity_type, entity_id,
		                          run_id, model, prompt_ver, input_digest, verdict, score,
		                          confidence, needs_human, injection, latency_ms, tokens_in,
		                          tokens_out, cache_hit, cost_usd, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, fmtTime(createdAt), d.AccountID, d.Task, d.EntityType, d.EntityID,
		d.RunID, d.Model, d.PromptVer, d.InputDigest, d.Verdict, d.Score,
		d.Confidence, boolInt(d.NeedsHuman), boolInt(d.Injection), d.LatencyMS,
		d.TokensIn, d.TokensOut, boolInt(d.CacheHit), d.CostUSD, d.Outcome)
	if err != nil {
		return fmt.Errorf("store: AIDecisionsAdd: %w", err)
	}
	return nil
}

// AIDecisionsByEntity returns the account's decisions for one entity, newest
// first, limited to limit.
func (db *DB) AIDecisionsByEntity(ctx context.Context, accountID, task, entityType, entityID string, limit int) ([]AIDecision, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, account_id, task, entity_type, entity_id, run_id, model,
		       prompt_ver, input_digest, verdict, score, confidence, needs_human,
		       injection, latency_ms, tokens_in, tokens_out, cache_hit, outcome, cost_usd
		FROM ai_decisions
		WHERE account_id = ? AND task = ? AND entity_type = ? AND entity_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, accountID, task, entityType, entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: AIDecisionsByEntity: %w", err)
	}
	defer rows.Close()

	var out []AIDecision
	for rows.Next() {
		d, err := scanAIDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("store: AIDecisionsByEntity scan: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// AIDecisionsSumTokensDay sums tokens_in/tokens_out for the account's
// decisions created on day ("YYYY-MM-DD", UTC prefix match).
func (db *DB) AIDecisionsSumTokensDay(ctx context.Context, accountID, day string) (int, int, error) {
	var in, out int
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		FROM ai_decisions
		WHERE account_id = ? AND substr(created_at, 1, 10) = ?`,
		accountID, day).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("store: AIDecisionsSumTokensDay: %w", err)
	}
	return in, out, nil
}

// AIDecisionsSumTokensDayAll sums tokens and cost globally — across ALL
// accounts — for decisions created on day ("YYYY-MM-DD", UTC prefix match).
// tokens = SUM(tokens_in + tokens_out): the consumer is the daily budget
// guard capping total consumption (internal/ai runner compares it with
// budget.DailyTokens); cost = SUM(cost_usd). The per-account
// AIDecisionsSumTokensDay is unchanged.
func (db *DB) AIDecisionsSumTokensDayAll(ctx context.Context, day string) (int, float64, error) {
	var tokens int
	var cost float64
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(tokens_in + tokens_out), 0), COALESCE(SUM(cost_usd), 0)
		FROM ai_decisions
		WHERE substr(created_at, 1, 10) = ?`, day).Scan(&tokens, &cost)
	if err != nil {
		return 0, 0, fmt.Errorf("store: AIDecisionsSumTokensDayAll: %w", err)
	}
	return tokens, cost, nil
}

func scanAIDecision(rs rowScanner) (*AIDecision, error) {
	var d AIDecision
	var score, confidence sql.NullFloat64
	var needsHuman, injection, cacheHit sql.NullInt64
	var created sql.NullString
	if err := rs.Scan(&d.ID, &created, &d.AccountID, &d.Task, &d.EntityType, &d.EntityID,
		&d.RunID, &d.Model, &d.PromptVer, &d.InputDigest, &d.Verdict, &score, &confidence,
		&needsHuman, &injection, &d.LatencyMS, &d.TokensIn, &d.TokensOut, &cacheHit,
		&d.Outcome, &d.CostUSD); err != nil {
		return nil, err
	}
	if score.Valid {
		f := score.Float64
		d.Score = &f
	}
	if confidence.Valid {
		f := confidence.Float64
		d.Confidence = &f
	}
	d.NeedsHuman = needsHuman.Int64 != 0
	d.Injection = injection.Int64 != 0
	d.CacheHit = cacheHit.Int64 != 0
	var err error
	if d.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	return &d, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
