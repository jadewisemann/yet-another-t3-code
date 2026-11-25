package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// registerHandlers lists the methods this server implements. Every other
// method in spec/generated/rpc.json answers "Unknown request tag", the same
// defect upstream sends for a method it does not have; spec/coverage.md
// tracks which phase implements which method.
func (s *Server) registerHandlers() map[string]Handler {
	return map[string]Handler{
		"orchestration.subscribeShell":  {Stream: true, Run: s.subscribeShell},
		"orchestration.subscribeThread": {Stream: true, Run: s.subscribeThread},
		"orchestration.dispatchCommand": {Unary: s.dispatchCommand},
		"projects.mutate":               {Unary: s.mutateProject},
		"orchestration.launchThread":    {Unary: s.launchThread},
		"server.getConfig":              {Unary: s.getConfig},
	}
}

// ImplementedMethods lists the RPC methods this server answers, sorted.
func ImplementedMethods() []string {
	var s Server
	out := make([]string, 0)
	for m := range s.registerHandlers() {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func strPtr(s string) *string { return &s }

// --- projects.mutate (spec/behavior/shell.md SHELL-4) -----------------------

type projectCommand struct {
	Type                  string          `json:"type"`
	CommandID             string          `json:"commandId"`
	ProjectID             string          `json:"projectId"`
	Title                 string          `json:"title"`
	WorkspaceRoot         string          `json:"workspaceRoot"`
	DefaultModelSelection json.RawMessage `json:"defaultModelSelection"`
	Scripts               json.RawMessage `json:"scripts"`
}

func projectError(cmd projectCommand, msg string) error {
	return &TypedError{Tag: "ProjectMutationError", Fields: map[string]any{"commandId": cmd.CommandID, "message": msg}}
}

func (s *Server) mutateProject(ctx context.Context, payload json.RawMessage) (any, error) {
	var cmd projectCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	if cmd.Type != "project.create" {
		return nil, fmt.Errorf("yatt-server does not implement project command %q yet", cmd.Type)
	}
	now := store.Now()
	p := store.Project{
		ID: cmd.ProjectID, Title: cmd.Title, WorkspaceRoot: cmd.WorkspaceRoot,
		DefaultModelSelection: cmd.DefaultModelSelection, Scripts: cmd.Scripts,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.store.Decide(ctx, cmd.CommandID, func(st *store.State) (string, any, error) {
		if _, ok := st.Projects[cmd.ProjectID]; ok {
			return "", nil, projectError(cmd, "Project already exists: "+cmd.ProjectID)
		}
		return store.ProjectCreated, p, nil
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return nil, err
	}
	var out map[string]any
	s.store.Read(func(st *store.State, _ int64) {
		if cur, ok := st.Projects[cmd.ProjectID]; ok {
			out = renderProject(cur)
		}
	})
	return out, nil
}

// --- orchestration.dispatchCommand (spec/behavior/threads.md) ---------------

type threadCommand struct {
	Type            string          `json:"type"`
	CommandID       string          `json:"commandId"`
	ThreadID        string          `json:"threadId"`
	ProjectID       string          `json:"projectId"`
	Title           *string         `json:"title"`
	CreatedBy       string          `json:"createdBy"`
	CreationSource  string          `json:"creationSource"`
	ModelSelection  json.RawMessage `json:"modelSelection"`
	RuntimeMode     string          `json:"runtimeMode"`
	InteractionMode string          `json:"interactionMode"`
	Branch          *string         `json:"branch"`
	WorktreePath    *string         `json:"worktreePath"`
	VisitedAt       string          `json:"visitedAt"`
}

func dispatchError(cmd threadCommand, msg string) error {
	return &TypedError{Tag: "OrchestrationV2DispatchCommandError", Fields: map[string]any{
		"commandId": cmd.CommandID, "commandType": cmd.Type, "message": msg,
	}}
}

// threadNotFound is the error upstream sends for a command on a missing
// thread, down to its nested cause (scenario dispatch-errors).
func threadNotFound(cmd threadCommand) error {
	inner := "No orchestration projection exists for thread " + cmd.ThreadID + "."
	return &TypedError{Tag: "OrchestrationV2DispatchCommandError", Fields: map[string]any{
		"commandId": cmd.CommandID, "commandType": cmd.Type, "message": inner, "detail": inner,
		"cause": map[string]any{
			"name":    "OrchestratorProjectionError",
			"message": "Failed to load orchestration projection for thread " + cmd.ThreadID + ".",
			"cause":   map[string]any{"name": "ProjectionStoreThreadNotFoundError", "message": inner},
		},
	}}
}

func (s *Server) dispatchCommand(ctx context.Context, payload json.RawMessage) (any, error) {
	var cmd threadCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	now := store.Now()
	needThread := func(st *store.State) (*store.Thread, error) {
		t, ok := st.Threads[cmd.ThreadID]
		if !ok {
			return nil, threadNotFound(cmd)
		}
		return t, nil
	}
	e, err := s.store.Decide(ctx, cmd.CommandID, func(st *store.State) (string, any, error) {
		switch cmd.Type {
		case "thread.create":
			// Upstream accepts a thread for a project that does not exist
			// (THREAD-2, scenario dispatch-errors).
			if _, ok := st.Threads[cmd.ThreadID]; ok {
				return "", nil, dispatchError(cmd, "Thread already exists: "+cmd.ThreadID)
			}
			title := ""
			if cmd.Title != nil {
				title = *cmd.Title
			}
			return store.ThreadCreated, store.Thread{
				ID: cmd.ThreadID, ProjectID: cmd.ProjectID, Title: title,
				CreatedBy: cmd.CreatedBy, CreationSource: cmd.CreationSource,
				ModelSelection: cmd.ModelSelection, RuntimeMode: cmd.RuntimeMode,
				InteractionMode: cmd.InteractionMode, Branch: cmd.Branch, WorktreePath: cmd.WorktreePath,
				CreatedAt: now, UpdatedAt: now,
			}, nil
		case "thread.metadata.update":
			if _, err := needThread(st); err != nil {
				return "", nil, err
			}
			return store.ThreadMetadataUpdated, store.ThreadMetadata{
				ThreadID: cmd.ThreadID, Title: cmd.Title, Branch: cmd.Branch, WorktreePath: cmd.WorktreePath, At: now,
			}, nil
		case "thread.visit":
			if _, err := needThread(st); err != nil {
				return "", nil, err
			}
			at := cmd.VisitedAt
			if at == "" {
				at = now
			}
			return store.ThreadVisited, store.ThreadRef{ThreadID: cmd.ThreadID, At: at}, nil
		case "thread.archive", "thread.unarchive":
			t, err := needThread(st)
			if err != nil {
				return "", nil, err
			}
			if cmd.Type == "thread.archive" {
				if t.ArchivedAt != nil {
					return "", nil, dispatchError(cmd, "Thread is already archived: "+cmd.ThreadID)
				}
				return store.ThreadArchived, store.ThreadRef{ThreadID: cmd.ThreadID, At: now}, nil
			}
			if t.ArchivedAt == nil {
				return "", nil, dispatchError(cmd, "Thread is not archived: "+cmd.ThreadID)
			}
			return store.ThreadUnarchived, store.ThreadRef{ThreadID: cmd.ThreadID, At: now}, nil
		default:
			return "", nil, fmt.Errorf("yatt-server does not implement command %q yet", cmd.Type)
		}
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return nil, err
	}
	return map[string]any{"sequence": e.Sequence}, nil
}

// --- orchestration.subscribeShell (spec/behavior/shell.md) -----------------

type subscribeInput struct {
	ThreadID                string `json:"threadId"`
	RequestCompletionMarker bool   `json:"requestCompletionMarker"`
}

func (s *Server) subscribeShell(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	var in subscribeInput
	_ = json.Unmarshal(payload, &in)
	var first map[string]any
	events, cancel := s.store.Subscribe(func(st *store.State, seq int64) {
		first = shellSnapshot(st, seq, "")
	})
	defer cancel()
	if err := emit(map[string]any{"kind": "snapshot", "snapshot": first}); err != nil {
		return err
	}
	if in.RequestCompletionMarker {
		if err := emit(map[string]any{"kind": "synchronized"}); err != nil {
			return err
		}
	}
	// Workspace roots whose repository identity this stream has reported
	// (SHELL-8). Upstream resolves it when a project's first thread appears
	// and then re-sends the snapshot with the root listed.
	resolved := map[string]bool{}
	for {
		var e store.Event
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok = <-events:
			if !ok {
				return errors.New("subscriber fell behind")
			}
		}
		var values []any
		s.store.Read(func(st *store.State, _ int64) {
			values = shellValues(st, e, resolved)
		})
		for _, v := range values {
			if err := emit(v); err != nil {
				return err
			}
		}
	}
}

// shellValues maps one event to the values the shell stream sends for it.
// It reads the current state, which may already include later events; the
// values describe the entity as it is now, as upstream's do.
func shellValues(st *store.State, e store.Event, resolved map[string]bool) []any {
	var ref struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(e.Data, &ref)
	threadID := ref.ThreadID
	if threadID == "" {
		threadID = ref.ID
	}
	switch e.Type {
	case store.ProjectCreated:
		p := st.Projects[ref.ID]
		return []any{map[string]any{"kind": "project.updated", "sequence": e.Sequence, "project": renderShellProject(p)}}
	case store.ThreadCreated, store.ThreadMetadataUpdated, store.ThreadUnarchived:
		t := st.Threads[threadID]
		var out []any
		if e.Type == store.ThreadCreated {
			if p := st.Projects[t.ProjectID]; p != nil && !resolved[p.WorkspaceRoot] {
				resolved[p.WorkspaceRoot] = true
				out = append(out, map[string]any{
					"kind":                            "snapshot",
					"snapshot":                        shellSnapshot(st, e.Sequence, t.ID),
					"resolvedRepositoryIdentityRoots": []string{p.WorkspaceRoot},
				})
			}
		}
		if t.ArchivedAt != nil {
			return out
		}
		return append(out, map[string]any{"kind": "thread.updated", "location": "active", "sequence": e.Sequence, "thread": renderShellThread(t)})
	case store.ThreadArchived:
		return []any{map[string]any{"kind": "thread.removed", "location": "active", "sequence": e.Sequence, "threadId": threadID}}
	}
	return nil // thread.visited and anything else: no shell value (SHELL-7)
}

// --- orchestration.subscribeThread (spec/behavior/threads.md THREAD-4) -----

func (s *Server) subscribeThread(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	var in subscribeInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	var first map[string]any
	var seq int64
	events, cancel := s.store.Subscribe(func(st *store.State, sq int64) {
		if t, ok := st.Threads[in.ThreadID]; ok {
			first = renderThreadProjection(t)
		}
		seq = sq
	})
	defer cancel()
	if first == nil {
		return &TypedError{Tag: "OrchestrationV2GetThreadProjectionError", Fields: map[string]any{
			"threadId": in.ThreadID, "message": "Thread not found: " + in.ThreadID,
		}}
	}
	if err := emit(map[string]any{"kind": "snapshot", "snapshotSequence": seq, "projection": first}); err != nil {
		return err
	}
	if in.RequestCompletionMarker {
		if err := emit(map[string]any{"kind": "synchronized"}); err != nil {
			return err
		}
	}
	// Turn and message events arrive with providers (P2).
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return errors.New("subscriber fell behind")
			}
		}
	}
}

// --- orchestration.launchThread (spec/behavior/threads.md THREAD-6) ---------

type launchInput struct {
	CommandID         string          `json:"commandId"`
	CreationSource    *string         `json:"creationSource"`
	ThreadID          *string         `json:"threadId"`
	ProjectID         string          `json:"projectId"`
	Title             string          `json:"title"`
	ModelSelection    json.RawMessage `json:"modelSelection"`
	RuntimeMode       string          `json:"runtimeMode"`
	InteractionMode   string          `json:"interactionMode"`
	WorkspaceStrategy struct {
		Type   string  `json:"type"`
		Branch *string `json:"branch"`
	} `json:"workspaceStrategy"`
	InitialMessage json.RawMessage `json:"initialMessage"`
}

func (s *Server) launchThread(ctx context.Context, payload json.RawMessage) (any, error) {
	var in launchInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.WorkspaceStrategy.Type != "root" {
		return nil, fmt.Errorf("yatt-server does not implement workspace strategy %q yet", in.WorkspaceStrategy.Type)
	}
	if len(in.InitialMessage) > 0 && string(in.InitialMessage) != "null" {
		return nil, errors.New("yatt-server cannot run agents yet, so it cannot launch a thread with a first message (phase P2)")
	}
	threadID := ""
	if in.ThreadID != nil && *in.ThreadID != "" {
		threadID = *in.ThreadID
	} else {
		id, err := newUUID()
		if err != nil {
			return nil, err
		}
		// Upstream's shape for server-chosen ids (scenario launch-thread).
		threadID = "thread:project:" + in.ProjectID + ":" + id
	}
	source := "web"
	if in.CreationSource != nil {
		source = *in.CreationSource
	}
	now := store.Now()
	_, err := s.store.Decide(ctx, in.CommandID, func(st *store.State) (string, any, error) {
		p, ok := st.Projects[in.ProjectID]
		if !ok {
			return "", nil, &TypedError{Tag: "OrchestrationV2DispatchCommandError", Fields: map[string]any{
				"commandId": in.CommandID, "commandType": "thread.launch", "message": "Project not found: " + in.ProjectID,
			}}
		}
		branch := in.WorkspaceStrategy.Branch
		if branch == nil {
			branch = gitBranch(p.WorkspaceRoot)
		}
		return store.ThreadCreated, store.Thread{
			ID: threadID, ProjectID: in.ProjectID, Title: in.Title, CreatedBy: "user", CreationSource: source,
			ModelSelection: in.ModelSelection, RuntimeMode: in.RuntimeMode, InteractionMode: in.InteractionMode,
			Branch: branch, CreatedAt: now, UpdatedAt: now,
		}, nil
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return nil, err
	}
	var projection map[string]any
	s.store.Read(func(st *store.State, _ int64) {
		if t, ok := st.Threads[threadID]; ok {
			projection = renderThreadProjection(t)
		}
	})
	return map[string]any{"threadId": threadID, "resumed": false, "projection": projection}, nil
}

// gitBranch reads the checked-out branch of a repository without running
// git, or nil when root is not a repository or HEAD is detached.
func gitBranch(root string) *string {
	data, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		return nil
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/")
	if !ok {
		return nil
	}
	return &ref
}
