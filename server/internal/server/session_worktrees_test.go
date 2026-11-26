package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

func TestSessionWorktreeDiffs(t *testing.T) {
	s, err := New(Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := git(dir, checkpointEnv, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	write(filepath.Join(repo, "a.txt"), "a\n")
	run(repo, "add", "a.txt")
	run(repo, "commit", "-q", "-m", "first")

	const thread = "thread-1"
	r := &runState{srv: s, cwd: repo, ids: runIDs{thread: thread, ordinal: 1}}

	// Turn 1: the agent makes a worktree and edits it.
	s.beginWorktreeTurn(r)
	wt := filepath.Join(t.TempDir(), "agent-wt")
	run(repo, "worktree", "add", "-q", "-b", "agent/fix", wt)
	write(filepath.Join(wt, "a.txt"), "a\nfrom the worktree\n")
	write(filepath.Join(wt, "new.txt"), "untracked\n")
	s.endWorktreeTurn(r)

	// Turn 2: it edits the worktree again.
	r.ids.ordinal = 2
	s.beginWorktreeTurn(r)
	write(filepath.Join(wt, "new.txt"), "untracked\nturn two\n")
	s.endWorktreeTurn(r)

	one := s.sessionWorktreeDiffs(thread, repo, 0, 1, true)
	if len(one) != 1 {
		t.Fatalf("turn 1: %v", one)
	}
	d1 := one[0].(map[string]any)
	if d1["branch"] != "agent/fix" || !strings.Contains(d1["diff"].(string), "+from the worktree") ||
		!strings.Contains(d1["diff"].(string), "b/new.txt") {
		t.Fatalf("turn 1 diff: %v", d1)
	}
	two := s.sessionWorktreeDiffs(thread, repo, 1, 2, true)
	if len(two) != 1 {
		t.Fatalf("turn 2: %v", two)
	}
	d2 := two[0].(map[string]any)["diff"].(string)
	if !strings.Contains(d2, "+turn two") || strings.Contains(d2, "from the worktree") {
		t.Fatalf("turn 2 diff should hold only turn 2: %s", d2)
	}
	all := s.sessionWorktreeDiffs(thread, repo, 0, 2, true)
	if d := all[0].(map[string]any)["diff"].(string); !strings.Contains(d, "+from the worktree") || !strings.Contains(d, "+turn two") {
		t.Fatalf("all turns: %s", d)
	}

	// A worktree made outside a turn of this thread is not its own.
	other := filepath.Join(t.TempDir(), "other-wt")
	run(repo, "worktree", "add", "-q", "-b", "other", other)
	r.ids.ordinal = 3
	s.beginWorktreeTurn(r)
	s.endWorktreeTurn(r)
	if f := s.loadSessionWorktrees(thread); len(f.Worktrees) != 1 {
		t.Fatalf("worktrees = %+v", f.Worktrees)
	}
}

func TestSettleAndSection(t *testing.T) {
	s, err := New(Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dispatch := func(cmd map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(cmd)
		if _, err := s.dispatchCommand(ctx, raw); err != nil {
			t.Fatalf("%v: %v", cmd["type"], err)
		}
	}
	dispatch(map[string]any{"type": "thread.create", "commandId": "c1", "threadId": "t1", "projectId": "p1",
		"title": "x", "modelSelection": map[string]any{}, "runtimeMode": "full-access", "interactionMode": "default"})
	shell := func() map[string]any {
		var m map[string]any
		s.store.Read(func(st *store.State, _ int64) { m = renderShellThread(st.Threads["t1"]) })
		return m
	}
	if m := shell(); m["settledAt"] != nil || m["settledOverride"] != nil || m["section"] != nil {
		t.Fatalf("new thread: %v %v %v", m["settledAt"], m["settledOverride"], m["section"])
	}
	dispatch(map[string]any{"type": "thread.settle", "commandId": "c2", "threadId": "t1"})
	dispatch(map[string]any{"type": "thread.section.set", "commandId": "c3", "threadId": "t1", "section": "  Reviews "})
	if m := shell(); m["settledAt"] == nil || m["settledOverride"] != "settled" || m["section"] != "Reviews" {
		t.Fatalf("settled: %v %v %v", m["settledAt"], m["settledOverride"], m["section"])
	}
	dispatch(map[string]any{"type": "thread.unsettle", "commandId": "c4", "threadId": "t1"})
	dispatch(map[string]any{"type": "thread.section.set", "commandId": "c5", "threadId": "t1", "section": nil})
	if m := shell(); m["settledAt"] != nil || m["settledOverride"] != "active" || m["unsettledAt"] == nil {
		t.Fatalf("unsettled: %v", m)
	} else if _, ok := m["section"]; ok {
		t.Fatalf("section kept: %v", m["section"])
	}
}
