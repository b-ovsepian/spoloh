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
	// KindAlertSuppressed is an air raid alert this device deliberately did
	// not sound, because the operator configured quiet hours to cover the
	// alerts category (config.QuietHours.AppliesTo, rules.ForEvent).
	//
	// It is its own kind rather than a KindAlert row with OK=false, because
	// those two answer different questions and the operator will be asking
	// the second one: "the siren failed" and "the siren was told to stay
	// quiet" are not the same event, and a page that renders them the same
	// way sends someone to check the speaker wiring over a setting. Nothing
	// else in this log records an announcement that never started, so
	// without this row the suppression leaves no trace at all — and a
	// device silent through a raid with nothing in its log is
	// indistinguishable from a broken one.
	//
	// Recorded with OK=false. Nothing failed, but a raid that went
	// unannounced is not a healthy outcome, and false is what puts the row
	// in /log's failure styling — the marked edge, the ✕, the tinted row —
	// which is what makes it findable in a page of two hundred entries.
	KindAlertSuppressed = "alert_suppressed"
	// KindPlaybackTruncated is a sound that started and did not finish
	// because something more important arrived — an air raid alert cutting
	// into the minute of silence is the case this exists for — or because
	// the operator pressed stop.
	//
	// It is not KindPlaybackError: nothing failed, the device chose
	// correctly, and a row that sends someone to check the speaker wiring
	// over that is a worse answer than no row at all. Nor is it enough to
	// leave the announcement's own row standing on its own. That row is
	// written when the sound is queued, before a byte reaches the speaker,
	// so that a power cut mid-siren still leaves evidence the siren was
	// sounded — which means it cannot also say how the sound ended. This is
	// the half that can, and together they let the log distinguish three
	// outcomes instead of two: played, cut short, failed.
	//
	// Recorded with OK=false, like KindAlertSuppressed and for the same
	// reason: nothing went wrong, but a sound the device meant to make and
	// did not finish is not a healthy outcome, and false is what puts the
	// row in /log's failure styling where it can be found in a page of two
	// hundred entries. The reason travels in Err, which is what keeps that
	// styling from reading as a fault.
	KindPlaybackTruncated = "playback_truncated"
	// KindLogCleared is the one entry a Clear leaves behind. An empty log
	// cannot tell "this device has never done anything" apart from
	// "somebody erased the record", and on a device whose log is the only
	// account of what it did during a real air raid, that difference is the
	// whole point of having a log at all.
	KindLogCleared = "log_cleared"
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

// Clear empties the log and records that it happened, in that order — the
// record has to be written after the delete or the delete would take it.
//
// Concurrency is the part that matters here, not the SQL. Append is called
// from the engine and from the player's OnError while sounds are playing, and
// a clear must never cost the log an alert that arrived while it ran. Two
// things give that:
//
//   - The delete names the rows it will take, once, as "everything that
//     exists at this instant" (id <= MAX(id)). An Append either lands before
//     it and is cleared — which is what the operator asked for, it was in
//     the log when they pressed the button — or lands after it and survives.
//     There is no window in which a row is written and then deleted by a
//     decision taken before it existed. Open sets MaxOpenConns(1) today, so
//     a bare DELETE would behave the same way and no test here can tell the
//     two apart; the bound is written down anyway because the property this
//     needs is "take what was there", not "take everything", and the day
//     that connection limit changes is not the day to rediscover the
//     difference.
//   - Nothing here blocks an announcement. A concurrent Append waits on the
//     one connection for as long as the delete takes (SQLite's busy_timeout
//     is 5s, and this deletes at most `capacity` rows from a file that is
//     tens of kilobytes), and appending is never on the path that puts sound
//     on the speaker: the engine hands the request to the player first and
//     records afterwards.
//
// The table is emptied, not compacted. No VACUUM: it rewrites the whole
// database through a temporary copy, which on an SD card is real write
// amplification and needs the free space to do it, in exchange for
// reclaiming a file that prune already caps at `capacity` rows. SQLite
// reuses the freed pages for the next inserts anyway.
//
// Row ids deliberately keep climbing past a clear (AUTOINCREMENT). Recent
// orders by id and means "newest" by it, and the delete above reasons about
// id ordering; ids that restarted at 1 would make both wrong the moment a
// clear raced an append. Recent and Count need nothing else: after a clear
// the table holds exactly the one entry below, which is what both should
// report, and neither caches anything.
func (l *Log) Clear(at time.Time) error {
	if l == nil {
		return nil
	}
	if _, err := l.db.Exec(`DELETE FROM entries WHERE id <= (SELECT MAX(id) FROM entries)`); err != nil {
		return err
	}
	// The sweep counter counts toward a prune that has nothing left to do;
	// resetting it is not required for correctness, only for not spending an
	// SD card write on a pointless sweep right after emptying the table.
	l.mu.Lock()
	l.inserts = 0
	l.mu.Unlock()

	// Through Append, not a second INSERT: this entry has to have exactly the
	// shape every other entry has, or the one row that explains an empty log
	// is the one row that renders differently from all the others.
	return l.Append(Entry{At: at, Kind: KindLogCleared, OK: true})
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
		// Deliberately not .UTC(): the stored value is an unambiguous Unix
		// millisecond, and which location it is rendered in is a decision
		// for the presentation layer, not this package.
		e.At = time.UnixMilli(ms)
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
