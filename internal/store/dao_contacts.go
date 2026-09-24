package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ContactsUpsert inserts or updates a contact keyed by
// (account_id, employer_id, source); c.ID receives the row ID.
func (db *DB) ContactsUpsert(ctx context.Context, c *Contact) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO contacts (account_id, employer_id, source, name, role, phones, emails,
		                      messengers, raw, collected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, employer_id, source) DO UPDATE SET
			name         = excluded.name,
			role         = excluded.role,
			phones       = excluded.phones,
			emails       = excluded.emails,
			messengers   = excluded.messengers,
			raw          = excluded.raw,
			collected_at = excluded.collected_at
		RETURNING id`,
		c.AccountID, c.EmployerID, c.Source, c.Name, c.Role, c.Phones, c.Emails,
		c.Messengers, c.Raw, fmtTime(c.CollectedAt),
	).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("store: ContactsUpsert: %w", err)
	}
	return nil
}

// ContactsByEmployer returns stored contacts for one employer.
func (db *DB) ContactsByEmployer(ctx context.Context, accountID string, employerID int64) ([]Contact, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, employer_id, source, name, role, phones, emails,
		       messengers, raw, collected_at
		FROM contacts WHERE account_id = ? AND employer_id = ?
		ORDER BY id`, accountID, employerID)
	if err != nil {
		return nil, fmt.Errorf("store: ContactsByEmployer: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

// ContactsExport returns every contact of the account, ordered by employer.
func (db *DB) ContactsExport(ctx context.Context, accountID string) ([]Contact, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, employer_id, source, name, role, phones, emails,
		       messengers, raw, collected_at
		FROM contacts WHERE account_id = ?
		ORDER BY employer_id, id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: ContactsExport: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

func scanContacts(rows *sql.Rows) ([]Contact, error) {
	var out []Contact
	for rows.Next() {
		var c Contact
		var name, role sql.NullString
		var collected sql.NullString
		if err := rows.Scan(&c.ID, &c.AccountID, &c.EmployerID, &c.Source, &name, &role,
			&c.Phones, &c.Emails, &c.Messengers, &c.Raw, &collected); err != nil {
			return nil, err
		}
		c.Name = nullStr(name)
		c.Role = nullStr(role)
		var err error
		if c.CollectedAt, err = timeFromNull(collected); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
