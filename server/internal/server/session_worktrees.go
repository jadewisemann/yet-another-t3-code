package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Session worktrees (a YATT extension, spec/behavior/session-worktrees.md):
// git worktrees that an agent creates by itself during a turn, for example
// with `git worktree add` or Claude Code's own worktrees. The thread's
// checkpoints only cover its own folder, so the server notes which
// worktrees appeared during each turn and snapshots them at the start and
// end of every later turn. Turn diffs then include their changes.
//
// The notes live in userdata/session-worktrees/<key>.json, outside the
// event log, so they change nothing upstream clients see.

type sessionWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	// Base is the commit the worktree's work starts from: where its HEAD
	// meets the thread folder's HEAD at the start of the turn that made it.
	Base        string `json:"base"`
	CreatedTurn int    `json:"createdTurn"`
}

// worktreeTurn is one turn's snapshots of one session worktree.
type worktreeTurn struct {
	Turn int    `json:"turn"`
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}

type sessionWorktreeFile struct {
	Worktrees []sessionWorktree `json:"worktrees"`
	Turns     []worktreeTurn    `json:"turns"`
}

// worktreeTurnStart is what a run noted before its turn started.
type worktreeTurnStart struct {
	known  map[string]bool   // worktrees of the repository, by real path
	head   string            // the thread folder's HEAD
	starts map[string]string // session worktree path -> its start snapshot
}

const sessionWorktreeRefsPrefix = "refs/yatt/session-worktrees"

func sessionWorktreeRef(threadID, path string, turn int, end string) string {
	sum := sha256.Sum256([]byte(threadID + "\x00" + path))
	key := base64.RawURLEncoding.EncodeToString([]byte(hex.EncodeToString(sum[:])[:32]))
	return fmt.Sprintf("%s/%s/turn/%d/%s", sessionWorktreeRefsPrefix, key, turn, end)
}

func (s *Server) sessionWorktreeFile(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return filepath.Join(UserData(s.cfg.BaseDir), "session-worktrees", hex.EncodeToString(sum[:])[:32]+".json")
}

func (s *Server) loadSessionWorktrees(threadID string) sessionWorktreeFile {
	var f sessionWorktreeFile
	if b, err := os.ReadFile(s.sessionWorktreeFile(threadID)); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f
}

func (s *Server) saveSessionWorktrees(threadID string, f sessionWorktreeFile) error {
	path := s.sessionWorktreeFile(threadID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// listWorktrees is every worktree of cwd's repository, by real path, main
// worktree first. It is nil when cwd is not in a git repository.
func listWorktrees(cwd string) []string {
	out, err := git(cwd, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, realPath(p))
		}
	}
	return paths
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// beginWorktreeTurn notes the repository's worktrees and snapshots the
// thread's session worktrees before a turn starts.
func (s *Server) beginWorktreeTurn(r *runState) {
	if r.cwd == "" {
		return
	}
	paths := listWorktrees(r.cwd)
	if paths == nil {
		return
	}
	start := &worktreeTurnStart{known: map[string]bool{}, starts: map[string]string{}}
	for _, p := range paths {
		start.known[p] = true
	}
	start.head, _ = git(r.cwd, nil, "rev-parse", "--verify", "-q", "HEAD")
	s.worktreeMu.Lock()
	f := s.loadSessionWorktrees(r.ids.thread)
	s.worktreeMu.Unlock()
	for _, w := range f.Worktrees {
		if !exists(w.Path) {
			continue
		}
		ref := sessionWorktreeRef(r.ids.thread, w.Path, r.ids.ordinal, "start")
		if ok, err := captureCheckpoint(w.Path, ref); err == nil && ok {
			start.starts[w.Path] = ref
		}
	}
	r.wtStart = start
}

// endWorktreeTurn adopts the worktrees that appeared during the turn and
// snapshots every session worktree after it.
func (s *Server) endWorktreeTurn(r *runState) {
	start := r.wtStart
	r.wtStart = nil
	if start == nil {
		return
	}
	paths := listWorktrees(r.cwd)
	// Worktrees that belong to the thread folder or to other threads are
	// not this session's.
	skip := map[string]bool{realPath(r.cwd): true}
	s.store.Read(func(st *store.State, _ int64) {
		for _, t := range st.Threads {
			if t.WorktreePath != nil && *t.WorktreePath != "" {
				skip[realPath(*t.WorktreePath)] = true
			}
		}
		for _, p := range st.Projects {
			skip[realPath(p.WorkspaceRoot)] = true
		}
	})
	if len(paths) > 0 {
		skip[paths[0]] = true // the main worktree
	}
	s.worktreeMu.Lock()
	defer s.worktreeMu.Unlock()
	f := s.loadSessionWorktrees(r.ids.thread)
	tracked := map[string]int{}
	for i, w := range f.Worktrees {
		tracked[w.Path] = i
	}
	for _, p := range paths {
		if start.known[p] || skip[p] {
			continue
		}
		if _, ok := tracked[p]; ok {
			continue
		}
		head, err := git(p, nil, "rev-parse", "--verify", "-q", "HEAD")
		if err != nil {
			continue
		}
		base := head
		if start.head != "" {
			if mb, err := git(p, nil, "merge-base", head, start.head); err == nil && mb != "" {
				base = mb
			}
		}
		f.Worktrees = append(f.Worktrees, sessionWorktree{Path: p, Base: base, CreatedTurn: r.ids.ordinal})
		tracked[p] = len(f.Worktrees) - 1
		start.starts[p] = base
	}
	if len(f.Worktrees) == 0 {
		return
	}
	// A turn that runs again (a retry) replaces its snapshots.
	kept := f.Turns[:0]
	for _, t := range f.Turns {
		if t.Turn != r.ids.ordinal {
			kept = append(kept, t)
		}
	}
	f.Turns = kept
	for i, w := range f.Worktrees {
		from := start.starts[w.Path]
		if from == "" || !exists(w.Path) {
			continue
		}
		if b, err := git(w.Path, nil, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
			f.Worktrees[i].Branch = b
		}
		to := sessionWorktreeRef(r.ids.thread, w.Path, r.ids.ordinal, "end")
		if ok, err := captureCheckpoint(w.Path, to); err != nil || !ok {
			continue
		}
		f.Turns = append(f.Turns, worktreeTurn{Turn: r.ids.ordinal, Path: w.Path, From: from, To: to})
	}
	_ = s.saveSessionWorktrees(r.ids.thread, f)
}

// sessionWorktreeDiffs is the patch of every session worktree over the
// turns after fromTurn up to toTurn. repo is any folder of the repository,
// used once a worktree is gone: the snapshots are refs of the repository.
func (s *Server) sessionWorktreeDiffs(threadID, repo string, fromTurn, toTurn int, ignoreWhitespace bool) []any {
	s.worktreeMu.Lock()
	f := s.loadSessionWorktrees(threadID)
	s.worktreeMu.Unlock()
	out := []any{}
	for _, w := range f.Worktrees {
		var first, last *worktreeTurn
		for i := range f.Turns {
			t := &f.Turns[i]
			if t.Path != w.Path || t.Turn <= fromTurn || t.Turn > toTurn {
				continue
			}
			if first == nil || t.Turn < first.Turn {
				first = t
			}
			if last == nil || t.Turn > last.Turn {
				last = t
			}
		}
		if first == nil {
			continue
		}
		from := first.From
		if fromTurn == 0 {
			from = w.Base
		}
		dir := w.Path
		if !exists(dir) {
			dir = repo
		}
		diff, err := checkpointPatch(dir, from, last.To, ignoreWhitespace)
		if err != nil || diff == "" {
			continue
		}
		out = append(out, map[string]any{"path": w.Path, "branch": w.Branch, "diff": diff})
	}
	return out
}

// --- yatt.getThreadPullRequests --------------------------------------------

// The pull requests of a thread are those whose head is the branch of the
// thread's folder or of one of its session worktrees, found with the
// GitHub CLI (`gh`), as the user is signed in to it.

type pullRequestsRequest struct {
	ThreadID string `json:"threadId"`
}

func ghBinary() string {
	if b := os.Getenv("YATT_GH_BIN"); b != "" {
		return b
	}
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	// An app opened from Finder has a short PATH.
	for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"} {
		if exists(p) {
			return p
		}
	}
	return ""
}

func (s *Server) getThreadPullRequests(ctx context.Context, payload json.RawMessage) (any, error) {
	var req pullRequestsRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	cwd := ""
	found := false
	s.store.Read(func(st *store.State, _ int64) {
		t := st.Threads[req.ThreadID]
		if t == nil {
			return
		}
		found = true
		if t.WorktreePath != nil && *t.WorktreePath != "" {
			cwd = *t.WorktreePath
		} else if p := st.Projects[t.ProjectID]; p != nil {
			cwd = p.WorkspaceRoot
		}
	})
	if !found {
		return nil, &TypedError{Tag: "YattPullRequestsError", Fields: map[string]any{"message": "Thread not found: " + req.ThreadID}}
	}
	result := map[string]any{"threadId": req.ThreadID, "pullRequests": []any{}}
	type head struct{ dir, branch string }
	var heads []head
	seen := map[string]bool{}
	addHead := func(dir string) {
		if dir == "" || !exists(dir) {
			return
		}
		b, err := git(dir, nil, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil || b == "" || seen[b] {
			return
		}
		seen[b] = true
		heads = append(heads, head{dir, b})
	}
	addHead(cwd)
	s.worktreeMu.Lock()
	f := s.loadSessionWorktrees(req.ThreadID)
	s.worktreeMu.Unlock()
	for _, w := range f.Worktrees {
		addHead(w.Path)
	}
	if len(heads) == 0 {
		return result, nil
	}
	gh := ghBinary()
	if gh == "" {
		result["error"] = "GitHub CLI (gh) is not installed"
		return result, nil
	}
	prs := []any{}
	urls := map[string]bool{}
	var firstErr string
	for _, h := range heads {
		list, err := ghPullRequests(ctx, gh, h.dir, h.branch)
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		for _, pr := range list {
			url, _ := pr["url"].(string)
			if urls[url] {
				continue
			}
			urls[url] = true
			pr["worktreePath"] = h.dir
			prs = append(prs, pr)
		}
	}
	result["pullRequests"] = prs
	if len(prs) == 0 && firstErr != "" {
		result["error"] = firstErr
	}
	return result, nil
}

func ghPullRequests(ctx context.Context, gh, dir, branch string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "pr", "list", "--head", branch, "--state", "all", "--limit", "5",
		"--json", "number,title,url,state,isDraft,headRefName,updatedAt")
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		if line, _, ok := strings.Cut(msg, "\n"); ok {
			msg = line
		}
		return nil, errors.New("gh: " + msg)
	}
	var list []map[string]any
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		return nil, err
	}
	// Open pull requests first, then the most recently updated.
	sort.SliceStable(list, func(a, b int) bool {
		oa, ob := list[a]["state"] == "OPEN", list[b]["state"] == "OPEN"
		if oa != ob {
			return oa
		}
		ua, _ := list[a]["updatedAt"].(string)
		ub, _ := list[b]["updatedAt"].(string)
		return ua > ub
	})
	return list, nil
}
