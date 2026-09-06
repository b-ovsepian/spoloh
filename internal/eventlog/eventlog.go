// Package eventlog records what the device did, with a hard row cap: this file
// lives on an SD card and must never grow without bound.
package eventlog

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; cgo would end ARM cross-compilation
)

const (
	KindAlert         = "alert"
	KindScheduled     = "scheduled"
	KindManual        = "manual"
	KindSourceFailure = "source_failure"
	KindConfigChange  = "config_change"
	KindPlaybackError = "playback_error"
)

// pruneEvery limits how often the capacity sweep runs; sweeping on every insert
// would be pure waste on a slow SD card.
const pruneEvery = 32

type Entry struct {
	ID      int64
	At      time.Time
	Kind    string
	EventID string
	Sound   string
	OK      bool
	Err     string
}

// Log is safe for concurrent use: Append may be called from multiple
// goroutines (e.g. the engine's state transitions and the player's OnError
// callback both append to the same Log).
//
// A nil *Log is valid and does nothing. The event log is a diagnostic: a
// corrupt database file or a read-only SD card must cost the operator their
// history, never the siren, so callers that fail to open one carry on with a
// nil Log rather than refusing to start.
type Log struct {
	db       *sql.DB
	capacity int

	mu      sync.Mutex // guards inserts and the decision to prune
	inserts int
}

func Open(path string, capacity int) (*Log, error) {
	if capacity < 1 {
		capacity = 1000
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer keeps SQLite happy and matches how the daemon actually uses it.
	db.SetMaxOpenConns(1)

	const schema = `
CREATE TABLE IF NOT EXISTS entries (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	at       INTEGER NOT NULL,
	kind     TEXT    NOT NULL,
	event_id TEXT    NOT NULL DEFAULT '',
	sound    TEXT    NOT NULL DEFAULT '',
	ok       INTEGER NOT NULL DEFAULT 1,
	err      TEXT    NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	l := &Log{db: db, capacity: capacity}
	// Prune unconditionally on every open, not just after pruneEvery inserts:
	// a board on unreliable mains power can restart more often than that, and
	// the in-process sweep counter starts back at zero on every restart. This
	// is the only place that re-establishes the cap regardless of how the
	// previous process's run ended.
	if err := l.prune(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) Append(e Entry) error {
	if l == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := l.db.Exec(
		`INSERT INTO entries (at, kind, event_id, sound, ok, err) VALUES (?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), e.Kind, e.EventID, e.Sound, boolToInt(e.OK), e.Err,
	)
	if err != nil {
		return err
	}

	// The counter and the sweep decision are shared state across goroutines;
	// the INSERT itself is not held under the lock since database/sql is
	// already safe for concurrent use on its own.
	l.mu.Lock()
	l.inserts++
	shouldPrune := l.inserts%pruneEvery == 0
	l.mu.Unlock()

	if shouldPrune {
		return l.prune()
	}
	return nil
}

func (l *Log) prune() error {
	_, err := l.db.Exec(
		`DELETE FROM entries WHERE id NOT IN (SELECT id FROM entries ORDER BY id DESC LIMIT ?)`,
		l.capacity,
	)
	return err
}

// Recent returns the newest n entries, newest first.
func (l *Log) Recent(n int) ([]Entry, error) {
	if l == nil {
		return nil, nil
	}
	if n < 1 {
		n = 50
	}
	rows, err := l.db.Query(
		`SELECT id, at, kind, event_id, sound, ok, err FROM entries ORDER BY id DESC LIMIT ?`, n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e     Entry
			ms    int64
			okInt int
		)
		if err := rows.Scan(&e.ID, &ms, &e.Kind, &e.EventID, &e.Sound, &okInt, &e.Err); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(ms).UTC()
		e.OK = okInt != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l *Log) Count() (int, error) {
	if l == nil {
		return 0, nil
	}
	var n int
	err := l.db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	return n, err
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
