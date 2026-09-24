package store

import (
	"context"
	"database/sql"
	"fmt"
)

// MessagesAdd inserts a chat message (message_id UNIQUE) and fills m.ID.
func (db *DB) MessagesAdd(ctx context.Context, m *Message) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO messages (chat_id, message_id, direction, author, text, sent_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		m.ChatID, m.MessageID, m.Direction, m.Author, m.Text, fmtTime(m.SentAt), nowOr(m.CreatedAt),
	).Scan(&m.ID)
	if err != nil {
		return fmt.Errorf("store: MessagesAdd: %w", err)
	}
	return nil
}

// MessagesHistory returns the chat's messages in chronological order.
func (db *DB) MessagesHistory(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, chat_id, message_id, direction, author, text, sent_at, created_at
		FROM messages WHERE chat_id = ?
		ORDER BY sent_at ASC, id ASC
		LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: MessagesHistory: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var author sql.NullString
		var sent, created sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatID, &m.MessageID, &m.Direction, &author, &m.Text, &sent, &created); err != nil {
			return nil, fmt.Errorf("store: MessagesHistory scan: %w", err)
		}
		m.Author = nullStr(author)
		var err error
		if m.SentAt, err = timeFromNull(sent); err != nil {
			return nil, err
		}
		if m.CreatedAt, err = timeFromNull(created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
