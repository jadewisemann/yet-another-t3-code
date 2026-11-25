package server

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Turn diffs: the patch between two checkpoints of a thread, addressed by
// turn count (spec/behavior/diffs.md), as upstream CheckpointDiffQuery.

const (
	checkpointDiffMaxBytes        = 10_000_000
	checkpointDiffTruncatedMarker = "\n\n[truncated]"
	maxPathspecChars              = 24_000
)

type turnDiffRequest struct {
	ThreadID         string `json:"threadId"`
	FromTurnCount    int    `json:"fromTurnCount"`
	ToTurnCount      int    `json:"toTurnCount"`
	IgnoreWhitespace *bool  `json:"ignoreWhitespace"`
}

type diffCheckpoint struct {
	scopeID, runID, ref string
	appRun, ordinal     int
	hasAppRun, ready    bool
}

type diffContext struct {
	runs        map[string]string // run id -> status
	checkpoints []diffCheckpoint
	scopes      map[string]string // scope id -> cwd
	rootScope   string
}

func (s *Server) getTurnDiff(ctx context.Context, payload json.RawMessage) (any, error) {
	var req turnDiffRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	out, err := s.turnDiff(req, "CheckpointDiffQuery.getTurnDiff")
	if err != nil {
		return nil, diffError("OrchestrationGetTurnDiffError", "Failed to load turn diff", err)
	}
	return out, nil
}

func (s *Server) getFullThreadDiff(ctx context.Context, payload json.RawMessage) (any, error) {
	var req turnDiffRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	req.FromTurnCount = 0
	out, err := s.turnDiff(req, "CheckpointDiffQuery.getFullThreadDiff")
	if err != nil {
		return nil, diffError("OrchestrationGetFullThreadDiffError", "Failed to load full thread diff", err)
	}
	return out, nil
}

// checkpointError is one of upstream's checkpointing errors, which travel
// as the cause of the RPC error.
type checkpointError struct{ name, message string }

func (e *checkpointError) Error() string { return e.message }

func diffError(tag, message string, cause error) error {
	c := map[string]any{"name": "Error", "message": cause.Error()}
	if ce, ok := cause.(*checkpointError); ok {
		c["name"] = ce.name
	}
	return &TypedError{Tag: tag, Fields: map[string]any{"message": message, "cause": c}}
}

func turnDiffResult(req turnDiffRequest, diff string) map[string]any {
	return map[string]any{"threadId": req.ThreadID, "fromTurnCount": req.FromTurnCount, "toTurnCount": req.ToTurnCount, "diff": diff}
}

func (s *Server) turnDiff(req turnDiffRequest, operation string) (map[string]any, error) {
	ignoreWhitespace := req.IgnoreWhitespace == nil || *req.IgnoreWhitespace
	if req.FromTurnCount == req.ToTurnCount {
		return turnDiffResult(req, ""), nil
	}
	var dc *diffContext
	s.store.Read(func(st *store.State, _ int64) {
		t := st.Threads[req.ThreadID]
		if t == nil {
			return
		}
		dc = &diffContext{runs: map[string]string{}, scopes: map[string]string{}}
		for _, r := range t.Proj.List("runs") {
			dc.runs[r.ID] = r.String("status")
		}
		for _, sc := range t.Proj.List("checkpointScopes") {
			dc.scopes[sc.ID] = sc.String("cwd")
			if sc.String("kind") == "root_run" && dc.rootScope == "" {
				dc.rootScope = sc.ID
			}
		}
		for _, c := range t.Proj.List("checkpoints") {
			appRun, hasAppRun := c.Field("appRunOrdinal").(float64)
			dc.checkpoints = append(dc.checkpoints, diffCheckpoint{
				scopeID: c.String("scopeId"), runID: c.String("runId"), ref: c.String("ref"),
				appRun: int(appRun), hasAppRun: hasAppRun, ordinal: int(c.Number("ordinalWithinScope")),
				ready: c.String("status") == "ready",
			})
		}
	})
	if dc == nil {
		return nil, &checkpointError{"CheckpointThreadNotFoundError",
			fmt.Sprintf("Checkpoint invariant violation in %s: Thread '%s' not found.", operation, req.ThreadID)}
	}
	var ready []diffCheckpoint
	maxTurn := 0
	for _, c := range dc.checkpoints {
		if c.ready && c.hasAppRun && c.runID != "" && dc.runs[c.runID] == "completed" {
			ready = append(ready, c)
			maxTurn = max(maxTurn, c.appRun)
		}
	}
	if req.ToTurnCount > maxTurn {
		return nil, &checkpointError{"CheckpointTurnRangeUnavailableError", fmt.Sprintf(
			"Checkpoint unavailable for thread %s turn %d: Turn diff range exceeds current turn count: requested %d, current %d.",
			req.ThreadID, req.ToTurnCount, req.ToTurnCount, maxTurn)}
	}
	unavailable := func(turn int) error {
		return &checkpointError{"CheckpointRefUnavailableError", fmt.Sprintf(
			"Checkpoint unavailable for thread %s turn %d: Checkpoint ref is unavailable for turn %d.", req.ThreadID, turn, turn)}
	}
	byTurn := func(n int) *diffCheckpoint {
		for i := range ready {
			if ready[i].appRun == n {
				return &ready[i]
			}
		}
		return nil
	}
	to := byTurn(req.ToTurnCount)
	if to == nil {
		return nil, unavailable(req.ToTurnCount)
	}
	cwd, ok := dc.scopes[to.scopeID]
	if !ok {
		return nil, &checkpointError{"CheckpointWorkspacePathMissingError", fmt.Sprintf(
			"Checkpoint invariant violation in %s: Workspace path missing for thread '%s' when computing turn diff.", operation, req.ThreadID)}
	}
	fromRef := ""
	switch {
	case req.FromTurnCount == 0:
		if dc.rootScope != "" {
			fromRef = checkpointRef(dc.rootScope, 0)
		}
	default:
		if req.FromTurnCount == req.ToTurnCount-1 {
			for _, c := range dc.checkpoints {
				if c.ready && c.scopeID == to.scopeID && c.ordinal == to.ordinal-1 {
					fromRef = c.ref
					break
				}
			}
		}
		if fromRef == "" {
			if c := byTurn(req.FromTurnCount); c != nil {
				fromRef = c.ref
			}
		}
	}
	if fromRef == "" {
		return nil, unavailable(req.FromTurnCount)
	}
	diff, err := checkpointPatch(cwd, fromRef, to.ref, ignoreWhitespace)
	if err != nil {
		return nil, err
	}
	return turnDiffResult(req, diff), nil
}

// checkpointPatch is the patch between two checkpoint refs, limited to the
// paths the turn wrote (see checkpointFiles) and to checkpointDiffMaxBytes.
func checkpointPatch(cwd, fromRef, toRef string, ignoreWhitespace bool) (string, error) {
	var files []numstatFile
	if out, err := gitRaw(cwd, "-c", "diff.relative=false", "-c", "diff.suppressBlankEmpty=false",
		"diff", "--numstat", "-z", "--no-color", "--no-ext-diff", "--no-textconv", "--submodule=short",
		fromRef+"^{commit}", toRef+"^{commit}"); err == nil {
		files = parseNumstat(out)
	}
	authored := authoredPaths(cwd, fromRef, toRef)
	var retained []numstatFile
	for _, f := range files {
		if authored == nil || authored[f.path] || (f.previous != "" && authored[f.previous]) {
			retained = append(retained, f)
		}
	}
	batches := [][]string{nil}
	if len(retained) != len(files) {
		batches = pathBatches(retained)
	}
	diff := ""
	remaining := checkpointDiffMaxBytes
	for _, paths := range batches {
		args := []string{"-c", "diff.relative=false", "-c", "diff.suppressBlankEmpty=false",
			"diff", "--patch", "--no-color", "--no-ext-diff", "--no-textconv", "--submodule=short",
			"--src-prefix=a/", "--dst-prefix=b/"}
		if ignoreWhitespace {
			args = append(args, "--ignore-all-space")
		}
		args = append(args, fromRef+"^{commit}", toRef+"^{commit}")
		if paths != nil {
			args = append(args, "--")
			for _, p := range paths {
				args = append(args, ":(top,literal)"+p)
			}
		}
		patch, err := gitRaw(cwd, args...)
		if err != nil {
			return "", err
		}
		diff += patch
		if len(patch) > remaining {
			cut := checkpointDiffMaxBytes - len(checkpointDiffTruncatedMarker)
			b := []byte(diff)[:cut]
			// Drop a rune the cut split, as a streaming decoder would.
			for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
				if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
					break
				}
				b = b[:len(b)-1]
			}
			return string(b) + checkpointDiffTruncatedMarker, nil
		}
		remaining -= len(patch)
	}
	return diff, nil
}

func pathBatches(files []numstatFile) [][]string {
	var batches [][]string
	var batch []string
	length := 0
	for _, f := range files {
		paths := []string{f.path}
		if f.previous != "" {
			paths = []string{f.previous, f.path}
		}
		size := 0
		for _, p := range paths {
			size += len(p) + 16
		}
		if len(batch) > 0 && length+size > maxPathspecChars {
			batches = append(batches, batch)
			batch, length = nil, 0
		}
		batch = append(batch, paths...)
		length += size
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}
