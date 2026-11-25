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
  occurred_at TEXT NOT NULL,
  batch       INTEGER
);
`

// Event is one entry of the log.
type Event struct {
	Sequence   int64
	CommandID  string
	Type       string
	Data       json.RawMessage
	OccurredAt string
	// Wire is how the thread stream shows the event, from Store.Render at
	// the moment it was applied. It is not stored.
	Wire any
}

// Store owns the log and the projections.
type Store struct {
	db    *sql.DB
	mu    sync.Mutex
	seq   int64
	state *State
	subs  map[int]chan Event
	next  int

	// Render, when set, describes each newly applied event for streams,
	// from the state just after it applied.
	Render func(st *State, e Event) any
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
	// Logs written before batches existed lack the column; every old event
	// was its own batch, which a NULL batch reads as.
	_, _ = db.Exec(`ALTER TABLE events ADD COLUMN batch INTEGER`)
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

// Draft is an event a command decides to append.
type Draft struct {
	Type string
	Data any
}

// Decide runs check against the current state and, if it returns an event
// type and data, appends that event. It all happens under one lock, so the
// check sees exactly the state the event applies to.
func (s *Store) Decide(ctx context.Context, commandID string, check func(st *State) (typ string, data any, err error)) (Event, error) {
	events, err := s.DecideMany(ctx, commandID, func(st *State) ([]Draft, error) {
		typ, data, err := check(st)
		if err != nil {
			return nil, err
		}
		return []Draft{{typ, data}}, nil
	})
	if len(events) == 0 {
		return Event{}, err
	}
	return events[len(events)-1], err
}

// DecideMany is Decide for a command that appends several events at once.
// They are written in one transaction; the command id is recorded on the
// first. A repeated command returns ErrDuplicate with the sequence of the
// last event the first attempt wrote.
func (s *Store) DecideMany(ctx context.Context, commandID string, check func(st *State) ([]Draft, error)) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if commandID != "" {
		var first int64
		err := s.db.QueryRowContext(ctx, `SELECT sequence FROM events WHERE command_id = ?`, commandID).Scan(&first)
		if err == nil {
			var last int64
			_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), ?) FROM events WHERE sequence >= ? AND batch = ?`, first, first, first).Scan(&last)
			return []Event{{Sequence: last, CommandID: commandID}}, ErrDuplicate
		}
	}
	drafts, err := check(s.state)
	if err != nil {
		return nil, err
	}
	if len(drafts) == 0 {
		return nil, nil
	}
	now := Now()
	events := make([]Event, len(drafts))
	for i, d := range drafts {
		raw, err := json.Marshal(d.Data)
		if err != nil {
			return nil, err
		}
		events[i] = Event{Sequence: s.seq + int64(i) + 1, Type: d.Type, Data: raw, OccurredAt: now}
	}
	events[0].CommandID = commandID
	// Apply to a copy first so a failing batch leaves nothing behind. The
	// copy shares thread projections, so v2 events are only checked here.
	trial := s.state.clone()
	for _, e := range events {
		if err := trial.trialApply(e); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, e := range events {
		var cmd any
		if e.CommandID != "" {
			cmd = e.CommandID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (sequence, command_id, type, data, occurred_at, batch) VALUES (?, ?, ?, ?, ?, ?)`,
			e.Sequence, cmd, e.Type, string(e.Data), e.OccurredAt, events[0].Sequence); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i := range events {
		_ = s.state.apply(events[i])
		if s.Render != nil {
			events[i].Wire = s.Render(s.state, events[i])
		}
		s.seq = events[i].Sequence
	}
	for _, e := range events {
		s.publish(e)
	}
	return events, nil
}

func (s *Store) publish(e Event) {
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
