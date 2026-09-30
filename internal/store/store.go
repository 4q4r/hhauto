// Package store provides the hhauto persistence layer: a pure-Go
// (CGO_ENABLED=0) SQLite database (modernc.org/sqlite) accessed only through
// typed DAO methods on *DB. No raw *sql.DB is exposed.
//
// Concurrency model: WAL journal, busy_timeout and MaxOpenConns(4) cover
// pool/cross-process access; an in-process write mutex serializes every
// write method so write-write contention (SQLITE_BUSY) cannot happen inside
// the application. Reads stay lock-free and run concurrently with writes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// ErrIntentPending is returned by IntentsBegin when an intent with the same
// key is still pending (idempotency-key conflict).
var ErrIntentPending = errors.New("store: intent with this key is already pending")

// ErrIntentDone is returned by IntentsBegin when an intent with the same key
// has already completed. A done intent is a hard barrier against re-running
// the side effect; lifting it (and re-arming the key) is a feature-level
// RECONCILE decision, never a store-level replacement.
var ErrIntentDone = errors.New("store: intent with this key is already done")

const (
	driverName           = "sqlite"
	defaultBusyTimeoutMS = 5000
	maxOpenConns         = 4
)

// Options configures Open.
type Options struct {
	// BusyTimeoutMS is the SQLite busy_timeout in milliseconds.
	// Zero or negative selects the default (5000).
	BusyTimeoutMS int
}

// DB is a handle to the SQLite database, safe for concurrent use.
type DB struct {
	conn          *sql.DB
	writeMu       sync.Mutex
	busyTimeoutMS int
	// Now, when set, overrides the wall clock used for ledger timestamps
	// (tests freeze it so day-bounded budgets see frozen-date rows). Nil
	// means time.Now.
	Now func() time.Time
}

// now returns the effective DB clock.
func (db *DB) now() time.Time {
	if db.Now != nil {
		return db.Now()
	}
	return time.Now()
}

// Open opens (creating if necessary) the SQLite database at path, applies
// pending migrations idempotently, and returns the handle.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("store: path is empty")
	}
	busy := opts.BusyTimeoutMS
	if busy <= 0 {
		busy = defaultBusyTimeoutMS
	}

	conn, err := sql.Open(driverName, buildDSN(path, busy))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	conn.SetMaxOpenConns(maxOpenConns)

	db := &DB{conn: conn, busyTimeoutMS: busy}
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := db.migrate(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the database handle.
func (db *DB) Close() error {
	if db == nil || db.conn == nil {
		return nil
	}
	return db.conn.Close()
}

// buildDSN builds a modernc.org/sqlite DSN. Pragmas travel as _pragma query
// parameters so every pooled connection receives them (busy_timeout and
// foreign_keys are per-connection in SQLite; WAL/NORMAL are re-asserted for
// completeness). busy_timeout is emitted first: the busy handler must be
// installed before journal_mode(WAL) can take locks (upstream applies it
// first since v1.57.0; the order here makes it explicit).
func buildDSN(path string, busyTimeoutMS int) string {
	v := url.Values{}
	v.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	v.Add("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "synchronous(NORMAL)")
	v.Add("_pragma", "foreign_keys(1)")
	u := url.URL{Scheme: "file", Path: path, RawQuery: v.Encode()}
	return u.String()
}

// --- time helpers: all timestamps are RFC3339 UTC TEXT ---

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func fmtTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func timeFromNull(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad timestamp %q: %w", ns.String, err)
	}
	return t, nil
}

func ptrTimeFromNull(ns sql.NullString) (*time.Time, error) {
	t, err := timeFromNull(ns)
	if err != nil {
		return nil, err
	}
	if t.IsZero() {
		return nil, nil
	}
	return &t, nil
}

func nowOr(t time.Time) string {
	if t.IsZero() {
		return fmtTime(time.Now())
	}
	return fmtTime(t)
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}
