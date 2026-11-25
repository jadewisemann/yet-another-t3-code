package store

import (
	"context"
	"errors"
	"testing"
)

func TestReplayAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	create := func(st *State) (string, any, error) {
		return ProjectCreated, Project{ID: "p1", Title: "one", WorkspaceRoot: "/w", CreatedAt: Now(), UpdatedAt: Now()}, nil
	}
	e, err := s.Decide(ctx, "c1", create)
	if err != nil || e.Sequence != 1 {
		t.Fatalf("first create: %v %+v", err, e)
	}
	if e, err := s.Decide(ctx, "c1", create); !errors.Is(err, ErrDuplicate) || e.Sequence != 1 {
		t.Fatalf("repeated command: %v %+v", err, e)
	}
	title := "renamed"
	_, err = s.Decide(ctx, "c2", func(st *State) (string, any, error) {
		return ThreadCreated, Thread{ID: "t1", ProjectID: "p1", Title: "a"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, "c3", func(st *State) (string, any, error) {
		return ThreadMetadataUpdated, ThreadMetadata{ThreadID: "t1", Title: &title, At: Now()}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// An event that cannot apply is not written.
	if _, err := s.Decide(ctx, "c4", func(st *State) (string, any, error) {
		return ThreadArchived, ThreadRef{ThreadID: "missing", At: Now()}, nil
	}); err == nil {
		t.Fatal("event for a missing thread was accepted")
	}
	s.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Read(func(st *State, seq int64) {
		if seq != 3 || st.Threads["t1"].Title != "renamed" || st.Projects["p1"].Title != "one" {
			t.Fatalf("replayed state wrong: seq=%d thread=%+v", seq, st.Threads["t1"])
		}
	})
}

func TestSubscribeSeesEventsAfterSnapshot(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var snapSeq int64 = -1
	ch, cancel := s.Subscribe(func(st *State, seq int64) { snapSeq = seq })
	defer cancel()
	_, _ = s.Decide(context.Background(), "", func(st *State) (string, any, error) {
		return ProjectCreated, Project{ID: "p"}, nil
	})
	if e := <-ch; snapSeq != 0 || e.Sequence != 1 {
		t.Fatalf("snapshot %d, event %d", snapSeq, e.Sequence)
	}
}
