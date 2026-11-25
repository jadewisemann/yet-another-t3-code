// Package store is the orchestration event log. Events are appended to
// SQLite and folded into in-memory projections; on start the projections are
// rebuilt by replaying the log. The schema is yatt-server's own: it does not
// follow upstream's migrations (see docs/go-port/PLAN.md, "데이터 폴더").
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
  sequence    INTEGER PRIMARY KEY,
  command_id  TEXT UNIQUE,
  type        TEXT NOT NULL,
  data        TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
`

// Event is one entry of the log.
type Event struct {
	Sequence   int64
	CommandID  string
	Type       string
	Data       json.RawMessage
	OccurredAt string
}

// Store owns the log and the projections.
type Store struct {
	db    *sql.DB
	mu    sync.Mutex
	seq   int64
	state *State
	subs  map[int]chan Event
	next  int
}

// TimeFormat is how every timestamp is written on the wire.
const TimeFormat = "2006-01-02T15:04:05.000Z"

func Now() string { return time.Now().UTC().Format(TimeFormat) }

// Open opens (or creates) userdata/yatt-state.sqlite and replays it.
func Open(userdata string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(userdata, "yatt-state.sqlite")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	s := &Store{db: db, state: newState(), subs: map[int]chan Event{}}
	rows, err := db.Query(`SELECT sequence, COALESCE(command_id, ''), type, data, occurred_at FROM events ORDER BY sequence`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.Sequence, &e.CommandID, &e.Type, &data, &e.OccurredAt); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		if err := s.state.apply(e); err != nil {
			return nil, fmt.Errorf("replaying event %d (%s): %w", e.Sequence, e.Type, err)
		}
		s.seq = e.Sequence
	}
	return s, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// ErrDuplicate is returned when a command id was already recorded.
var ErrDuplicate = errors.New("command already recorded")

// Decide runs check against the current state and, if it returns an event
// type and data, appends that event. It all happens under one lock, so the
// check sees exactly the state the event applies to.
func (s *Store) Decide(ctx context.Context, commandID string, check func(st *State) (typ string, data any, err error)) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if commandID != "" {
		var seq int64
		err := s.db.QueryRowContext(ctx, `SELECT sequence FROM events WHERE command_id = ?`, commandID).Scan(&seq)
		if err == nil {
			return Event{Sequence: seq, CommandID: commandID}, ErrDuplicate
		}
	}
	typ, data, err := check(s.state)
	if err != nil {
		return Event{}, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	e := Event{Sequence: s.seq + 1, CommandID: commandID, Type: typ, Data: raw, OccurredAt: Now()}
	// Apply to a copy first so a failing event leaves nothing behind.
	if err := s.state.clone().apply(e); err != nil {
		return Event{}, err
	}
	var cmd any
	if commandID != "" {
		cmd = commandID
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (sequence, command_id, type, data, occurred_at) VALUES (?, ?, ?, ?, ?)`,
		e.Sequence, cmd, e.Type, string(raw), e.OccurredAt); err != nil {
		return Event{}, err
	}
	_ = s.state.apply(e)
	s.seq = e.Sequence
	for id, ch := range s.subs {
		select {
		case ch <- e:
		default:
			// A subscriber that fell this far behind is dropped rather than
			// blocking every writer; its stream ends and the client resubscribes.
			close(ch)
			delete(s.subs, id)
		}
	}
	return e, nil
}

// Subscribe calls snapshot with the current state and sequence, and then
// delivers every later event on the returned channel. Nothing is missed or
// repeated between the snapshot and the first event.
func (s *Store) Subscribe(snapshot func(st *State, seq int64)) (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot(s.state, s.seq)
	ch := make(chan Event, 4096)
	id := s.next
	s.next++
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// Read calls f with the current state under the lock.
func (s *Store) Read(f func(st *State, seq int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.state, s.seq)
}
