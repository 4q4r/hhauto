package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChatsUpsert inserts or updates a chat keyed by (account_id, chatik_chat_id);
// c.ID receives the row ID. On conflict the conversation state is NOT
// clobbered: state and rejection_confidence change only via ChatsUpdateState
// (so re-discovering a chat never resets rejected/left/escalated/warmup_due).
// An empty State falls back to 'active' on insert.
func (db *DB) ChatsUpsert(ctx context.Context, c *Chat) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO chats (account_id, chatik_chat_id, topic_id, vacancy_id, employer_id,
		                   employer_name, state, rejection_confidence, last_message_at,
		                   last_outgoing_at, leave_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'active'), ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, chatik_chat_id) DO UPDATE SET
			topic_id         = excluded.topic_id,
			vacancy_id       = excluded.vacancy_id,
			employer_id      = excluded.employer_id,
			employer_name    = excluded.employer_name,
			last_message_at  = excluded.last_message_at,
			last_outgoing_at = excluded.last_outgoing_at,
			leave_count      = excluded.leave_count,
			updated_at       = excluded.updated_at
		RETURNING id`,
		c.AccountID, c.ChatikChatID, c.TopicID, c.VacancyID, c.EmployerID,
		c.EmployerName, c.State, c.RejectionConfidence, fmtTimeArg(c.LastMessageAt),
		fmtTimeArg(c.LastOutgoingAt), c.LeaveCount, nowOr(c.UpdatedAt),
	).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("store: ChatsUpsert: %w", err)
	}
	return nil
}

// ChatsUpdateState transitions the chat state. rejectionConfidence != nil
// updates rejection_confidence; nil leaves it unchanged.
func (db *DB) ChatsUpdateState(ctx context.Context, accountID, chatikChatID, state string, rejectionConfidence *float64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		UPDATE chats SET state = ?, rejection_confidence = COALESCE(?, rejection_confidence), updated_at = ?
		WHERE account_id = ? AND chatik_chat_id = ?`,
		state, rejectionConfidence, fmtTime(time.Now()), accountID, chatikChatID)
	if err != nil {
		return fmt.Errorf("store: ChatsUpdateState: %w", err)
	}
	return nil
}

// ChatsDueWarmup returns the account's warmup_due chats, oldest last
// outgoing message first, limited to limit.
func (db *DB) ChatsDueWarmup(ctx context.Context, accountID string, limit int) ([]Chat, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, chatik_chat_id, topic_id, vacancy_id, employer_id,
		       employer_name, state, rejection_confidence, last_message_at,
		       last_outgoing_at, leave_count, updated_at
		FROM chats
		WHERE account_id = ? AND state = 'warmup_due'
		ORDER BY last_outgoing_at
		LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ChatsDueWarmup: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ChatsDueWarmup scan: %w", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// chatsByChatikID is an in-package lookup used by tests.
func (db *DB) chatsByChatikID(ctx context.Context, accountID, chatikChatID string) (*Chat, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, chatik_chat_id, topic_id, vacancy_id, employer_id,
		       employer_name, state, rejection_confidence, last_message_at,
		       last_outgoing_at, leave_count, updated_at
		FROM chats WHERE account_id = ? AND chatik_chat_id = ?`, accountID, chatikChatID)
	c, err := scanChat(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: chatsByChatikID: %w", err)
	}
	return c, nil
}

func scanChat(rs rowScanner) (*Chat, error) {
	var c Chat
	var topic, employerName sql.NullString
	var vacancy, employer sql.NullInt64
	var confidence sql.NullFloat64
	var lastMsg, lastOut, updated sql.NullString
	if err := rs.Scan(&c.ID, &c.AccountID, &c.ChatikChatID, &topic, &vacancy, &employer,
		&employerName, &c.State, &confidence, &lastMsg, &lastOut, &c.LeaveCount, &updated); err != nil {
		return nil, err
	}
	c.TopicID = nullStr(topic)
	c.EmployerName = nullStr(employerName)
	if vacancy.Valid {
		v := vacancy.Int64
		c.VacancyID = &v
	}
	if employer.Valid {
		v := employer.Int64
		c.EmployerID = &v
	}
	if confidence.Valid {
		f := confidence.Float64
		c.RejectionConfidence = &f
	}
	var err error
	if c.LastMessageAt, err = ptrTimeFromNull(lastMsg); err != nil {
		return nil, err
	}
	if c.LastOutgoingAt, err = ptrTimeFromNull(lastOut); err != nil {
		return nil, err
	}
	if c.UpdatedAt, err = timeFromNull(updated); err != nil {
		return nil, err
	}
	return &c, nil
}
