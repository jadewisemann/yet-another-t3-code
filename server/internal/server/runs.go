package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/codex"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Runs: a message sent to a thread starts a run, which drives one provider
// turn (spec/behavior/runs.md). Every step is recorded as the v2 events
// upstream emits, in upstream's order, so the thread projection and the
// thread stream match (scenario message-dispatch).

//go:embed codex-capabilities.json
var codexCapabilities json.RawMessage

// encodeURIComponent escapes s as JavaScript's encodeURIComponent does.
func encodeURIComponent(s string) string {
	const keep = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// runIDs are the ids of one run's entities, in upstream's formats.
type runIDs struct {
	thread, run, attempt, root, providerThread, session, scope string
	ordinal                                                    int
}

func newRunIDs(threadID, instanceID string, ordinal int, providerThread string) runIDs {
	run := fmt.Sprintf("run:thread:%s:ordinal:%d", threadID, ordinal)
	if providerThread == "" {
		providerThread = "provider-thread:provider:codex:native-thread:" + encodeURIComponent("pending:"+run)
	}
	return runIDs{
		thread:         threadID,
		run:            run,
		attempt:        "run-attempt:run:" + encodeURIComponent(run) + ":attempt:1",
		root:           "node:run:" + encodeURIComponent(run) + ":root",
		providerThread: providerThread,
		session:        "provider-session:provider-instance:" + instanceID + ":shared",
		scope:          "checkpoint-scope:thread:" + threadID + ":name:root",
		ordinal:        ordinal,
	}
}

func (ids runIDs) checkpoint(n int) string {
	return fmt.Sprintf("checkpoint:scope:%s:name:%d", encodeURIComponent(ids.scope), n)
}

// envelope builds a v2 event. cause is the middle part of the event id:
// "command:<id>", "provider-session:<enc id>", or "" for the orchestrator.
func envelope(threadID, cause, typ string, payload any) (store.Draft, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return store.Draft{}, err
	}
	u, err := newUUID()
	if err != nil {
		return store.Draft{}, err
	}
	id := "event:thread:" + threadID + ":"
	if cause != "" {
		id += cause + ":"
	}
	return store.Draft{Type: typ, Data: &store.Envelope{
		ID: id + u, ThreadID: threadID, OccurredAt: store.Now(), Type: typ, Payload: raw,
	}}, nil
}

// event is a draft under construction.
type event struct {
	cause, typ, runID, nodeID, driver string
	payload                           any
}

func (r *runState) drafts(events ...event) ([]store.Draft, error) {
	out := make([]store.Draft, 0, len(events))
	for _, e := range events {
		d, err := envelope(r.ids.thread, e.cause, e.typ, e.payload)
		if err != nil {
			return nil, err
		}
		env := d.Data.(*store.Envelope)
		env.RunID, env.NodeID, env.Driver, env.ProviderInstanceID = e.runID, e.nodeID, e.driver, r.instance
		out = append(out, d)
	}
	return out, nil
}

func (r *runState) append(ctx context.Context, events ...event) error {
	drafts, err := r.drafts(events...)
	if err != nil {
		return err
	}
	_, err = r.srv.store.DecideMany(ctx, "", func(*store.State) ([]store.Draft, error) { return drafts, nil })
	return err
}

// runState is what the run goroutine knows about its run.
type runState struct {
	srv           *Server
	ids           runIDs
	instance      string
	model         string
	modelSel      json.RawMessage
	cwd           string
	title         string
	text          string
	userMessageID string
	requestedAt   string
	startedAt     string
	nativeThread  string
	turnID        string
	providerTurn  string
	turnStarted   string
	itemOrdinal   int
	hasSession    bool
	ptCreatedAt   string
	ptFirstRun    int
	baseline      *checkpointInfo // the scope's first checkpoint, when this run makes it
	parent        string          // the checkpoint the run's checkpoint follows
	nextCkpt      int
	stop          chan struct{}
	stopOnce      sync.Once
	mu            sync.Mutex // guards itemOrdinal and providerTurn, which stop requests read
	stopRequest   string     // the interrupt-request turn item, once stopped
}

type checkpointInfo struct {
	ordinal int
	ref     string
	status  string
	at      string
}

func (r *runState) runPayload(status string, completedAt any, checkpointID any) map[string]any {
	started := any(nil)
	if r.startedAt != "" {
		started = r.startedAt
	}
	return map[string]any{
		"id": r.ids.run, "threadId": r.ids.thread, "ordinal": r.ids.ordinal,
		"providerInstanceId": r.instance, "modelSelection": r.modelSel,
		"providerThreadId": r.ids.providerThread, "userMessageId": r.userMessageID,
		"rootNodeId": r.ids.root, "activeAttemptId": r.ids.attempt, "status": status,
		"queuePosition": nil, "requestedAt": r.requestedAt, "startedAt": started,
		"completedAt": completedAt, "checkpointId": checkpointID, "contextHandoffId": nil,
	}
}

func (r *runState) attemptPayload(status string, completedAt any) map[string]any {
	m := map[string]any{
		"id": r.ids.attempt, "runId": r.ids.run, "attemptOrdinal": 1, "rootNodeId": r.ids.root,
		"providerInstanceId": r.instance, "providerThreadId": r.ids.providerThread,
		"providerTurnId": nil, "reason": "initial", "status": status,
		"startedAt": nilIfEmpty(r.startedAt), "completedAt": completedAt,
	}
	if r.nativeThread != "" {
		m["nativeThreadId"] = r.nativeThread
	}
	return m
}

func (r *runState) rootNode(status string, completedAt any) map[string]any {
	return map[string]any{
		"id": r.ids.root, "threadId": r.ids.thread, "runId": r.ids.run, "parentNodeId": nil,
		"rootNodeId": r.ids.root, "kind": "root_turn", "status": status, "countsForRun": true,
		"providerThreadId": r.ids.providerThread, "providerTurnId": nil, "nativeItemRef": nil,
		"runtimeRequestId": nil, "checkpointScopeId": r.ids.scope,
		"startedAt": nilIfEmpty(r.startedAt), "completedAt": completedAt,
	}
}

func (r *runState) providerThread(status string) map[string]any {
	var native any
	if r.nativeThread != "" {
		native = map[string]any{"driver": "codex", "nativeId": r.nativeThread, "strength": "strong"}
	}
	return map[string]any{
		"id": r.ids.providerThread, "driver": "codex", "providerInstanceId": r.instance,
		"providerSessionId": r.ids.session, "appThreadId": r.ids.thread, "ownerNodeId": nil,
		"nativeThreadRef": native, "nativeConversationHeadRef": nil, "status": status,
		"firstRunOrdinal": r.ptFirstRun, "lastRunOrdinal": r.ids.ordinal, "handoffIds": []any{},
		"forkedFrom": nil, "pendingBackgroundTasks": []any{}, "contextUsage": nil,
		"nativeMetadata": nil, "goal": nil, "createdAt": r.ptCreatedAt, "updatedAt": store.Now(),
	}
}

func (r *runState) providerTurnPayload(status string, completedAt any) map[string]any {
	m := map[string]any{
		"id": r.providerTurn, "providerThreadId": r.ids.providerThread, "nodeId": r.ids.root,
		"runAttemptId":  r.ids.attempt,
		"nativeTurnRef": map[string]any{"driver": "codex", "nativeId": r.turnID, "strength": "strong"},
		"ordinal":       r.ids.ordinal, "status": status, "startedAt": r.turnStarted, "completedAt": completedAt,
	}
	if completedAt != nil {
		m["turnTokenUsage"] = map[string]any{"usageScope": "main_agent", "hasSubagents": false, "usageStatus": "unavailable"}
	}
	return m
}

func (r *runState) nextItemOrdinal() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.itemOrdinal++
	return r.ids.ordinal*1000000 + r.itemOrdinal
}

func (r *runState) currentTurn() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerTurn
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- message.dispatch ---------------------------------------------------------

type messageCommand struct {
	CommandID      string          `json:"commandId"`
	ThreadID       string          `json:"threadId"`
	MessageID      string          `json:"messageId"`
	Text           string          `json:"text"`
	Attachments    json.RawMessage `json:"attachments"`
	CreatedBy      string          `json:"createdBy"`
	CreationSource string          `json:"creationSource"`
	DispatchMode   *struct {
		Type string `json:"type"`
	} `json:"dispatchMode"`
}

func latestRun(t *store.Thread) *store.Entity {
	runs := t.Proj.List("runs")
	if len(runs) == 0 {
		return nil
	}
	return runs[len(runs)-1]
}

var activeRunStatuses = map[string]bool{"preparing": true, "queued": true, "starting": true, "running": true, "waiting": true}

func (s *Server) dispatchMessage(ctx context.Context, payload json.RawMessage) (any, error) {
	var cmd messageCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	tc := threadCommand{Type: "message.dispatch", CommandID: cmd.CommandID, ThreadID: cmd.ThreadID}
	var r *runState
	events, err := s.store.DecideMany(ctx, cmd.CommandID, func(st *store.State) ([]store.Draft, error) {
		t, ok := st.Threads[cmd.ThreadID]
		if !ok {
			return nil, threadNotFound(tc)
		}
		if run := latestRun(t); run != nil && activeRunStatuses[run.String("status")] {
			return nil, dispatchError(tc, "yatt-server cannot queue a message behind a running turn yet: "+cmd.ThreadID)
		}
		instance, _ := providerInstanceID(t).(string)
		if instance != "codex" {
			return nil, dispatchError(tc, fmt.Sprintf("yatt-server does not run provider %q yet", instance))
		}
		var sel struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(t.ModelSelection, &sel)
		cwd := ""
		if p := st.Projects[t.ProjectID]; p != nil {
			cwd = p.WorkspaceRoot
		}
		if t.WorktreePath != nil {
			cwd = *t.WorktreePath
		}
		ordinal := len(t.Proj.List("runs")) + 1
		existingPT := ""
		var pt *store.Entity
		for _, e := range t.Proj.List("providerThreads") {
			if e.String("appThreadId") == t.ID && e.String("driver") == "codex" {
				pt, existingPT = e, e.ID
			}
		}
		now := store.Now()
		r = &runState{
			srv: s, ids: newRunIDs(t.ID, instance, ordinal, existingPT), instance: instance,
			model: sel.Model, modelSel: t.ModelSelection, cwd: cwd, title: t.Title, text: cmd.Text,
			userMessageID: cmd.MessageID, requestedAt: now, ptCreatedAt: now, ptFirstRun: ordinal,
			hasSession: t.Proj.Get("providerSessions", "provider-session:provider-instance:"+instance+":shared") != nil,
			stop:       make(chan struct{}),
		}
		if pt != nil {
			r.ptCreatedAt = pt.String("createdAt")
			r.ptFirstRun = int(pt.Number("firstRunOrdinal"))
			if ref, ok := pt.Field("nativeThreadRef").(map[string]any); ok {
				r.nativeThread, _ = ref["nativeId"].(string)
			}
		}
		ckpts := 0
		for _, c := range t.Proj.List("checkpoints") {
			if c.String("scopeId") == r.ids.scope {
				ckpts++
				r.parent = c.ID
			}
		}
		r.nextCkpt = ckpts
		createdBy, source := cmd.CreatedBy, cmd.CreationSource
		attachments := cmd.Attachments
		if len(attachments) == 0 {
			attachments = json.RawMessage("[]")
		}
		cause := "command:" + cmd.CommandID
		visited := now
		threadDraft := store.Draft{Type: store.ThreadMetadataUpdated, Data: store.ThreadMetadata{ThreadID: t.ID, LastVisitedAt: &visited, At: now}}
		evs := []event{
			{cause: cause, typ: "provider-thread.updated", driver: "codex", payload: r.providerThread("not_loaded")},
			{cause: cause, typ: "run.created", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload("starting", nil, nil)},
			{cause: cause, typ: "run-attempt.created", runID: r.ids.run, nodeID: r.ids.root, payload: r.attemptPayload("pending", nil)},
			{cause: cause, typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode("pending", nil)},
		}
		if t.Proj.Get("checkpointScopes", r.ids.scope) == nil {
			evs = append(evs, event{cause: cause, typ: "checkpoint-scope.created", runID: r.ids.run, nodeID: r.ids.root, payload: map[string]any{
				"id": r.ids.scope, "threadId": t.ID, "runId": r.ids.run, "nodeId": r.ids.root, "parentScopeId": nil,
				"providerThreadId": r.ids.providerThread, "kind": "root_run", "ordinalWithinParent": 0,
				"advancesAppRunCount": true, "cwd": cwd, "createdAt": now,
			}})
		}
		evs = append(evs,
			event{cause: cause, typ: "message.updated", runID: r.ids.run, nodeID: r.ids.root, payload: map[string]any{
				"createdBy": createdBy, "creationSource": source, "id": cmd.MessageID, "threadId": t.ID,
				"runId": r.ids.run, "nodeId": r.ids.root, "role": "user", "text": cmd.Text,
				"attachments": attachments, "streaming": false, "createdAt": now, "updatedAt": now,
			}},
			event{cause: cause, typ: "turn-item.updated", runID: r.ids.run, nodeID: r.ids.root, payload: map[string]any{
				"id": "turn-item:message:" + cmd.MessageID, "threadId": t.ID, "runId": r.ids.run, "nodeId": r.ids.root,
				"providerThreadId": r.ids.providerThread, "providerTurnId": nil, "nativeItemRef": nil,
				"parentItemId": nil, "ordinal": r.nextItemOrdinal(), "status": "completed", "title": nil,
				"startedAt": now, "completedAt": now, "updatedAt": now, "createdBy": createdBy,
				"creationSource": source, "type": "user_message", "messageId": cmd.MessageID,
				"inputIntent": "turn_start", "text": cmd.Text, "attachments": attachments,
			}},
		)
		drafts, err := r.drafts(evs...)
		if err != nil {
			return nil, err
		}
		return append([]store.Draft{threadDraft}, drafts...), nil
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return nil, err
	}
	if err == nil && r != nil {
		s.runs.start(r)
	}
	return map[string]any{"sequence": events[len(events)-1].Sequence}, nil
}

// --- the run goroutine ----------------------------------------------------------

// runner owns the shared codex app-server process and the active runs.
type runner struct {
	srv    *Server
	mu     sync.Mutex
	client *codex.Client
	loaded map[string]bool      // native thread ids loaded in client
	active map[string]*runState // by app thread id
}

func newRunner(s *Server) *runner {
	return &runner{srv: s, loaded: map[string]bool{}, active: map[string]*runState{}}
}

func (rn *runner) start(r *runState) {
	rn.mu.Lock()
	rn.active[r.ids.thread] = r
	rn.mu.Unlock()
	go func() {
		defer func() {
			rn.mu.Lock()
			if rn.active[r.ids.thread] == r {
				delete(rn.active, r.ids.thread)
			}
			rn.mu.Unlock()
		}()
		if err := rn.run(r); err != nil {
			rn.srv.log.Warn("run failed", slog.String("run", r.ids.run), slog.String("error", err.Error()))
			rn.fail(r, err)
		}
	}()
}

// requestStop records a stop request for the thread's active run and
// interrupts its turn. It returns the request's event sequence, or false
// when no run is active.
func (rn *runner) requestStop(ctx context.Context, threadID, commandID string) (int64, bool, error) {
	rn.mu.Lock()
	r := rn.active[threadID]
	rn.mu.Unlock()
	if r == nil {
		return 0, false, nil
	}
	now := store.Now()
	id := "turn-item:run:" + encodeURIComponent(r.ids.run) + ":signal:interrupt-request"
	drafts, err := r.drafts(event{cause: "command:" + commandID, typ: "turn-item.updated", runID: r.ids.run, nodeID: r.ids.root, payload: map[string]any{
		"id": id, "threadId": r.ids.thread, "runId": r.ids.run, "nodeId": r.ids.root,
		"providerThreadId": r.ids.providerThread, "providerTurnId": nilIfEmpty(r.currentTurn()),
		"nativeItemRef": nil, "parentItemId": nil, "ordinal": r.nextItemOrdinal(), "status": "completed",
		"title": "Interrupt requested", "startedAt": now, "completedAt": now, "updatedAt": now,
		"type": "run_interrupt_request", "message": "Stopped by user",
	}})
	if err != nil {
		return 0, true, err
	}
	events, err := rn.srv.store.DecideMany(ctx, commandID, func(*store.State) ([]store.Draft, error) { return drafts, nil })
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return 0, true, err
	}
	if err == nil {
		r.mu.Lock()
		r.stopRequest = id
		r.mu.Unlock()
		r.stopOnce.Do(func() { close(r.stop) })
	}
	return events[len(events)-1].Sequence, true, nil
}

func (rn *runner) codexBinary() string {
	if b := os.Getenv("YATT_CODEX_BIN"); b != "" {
		return b
	}
	return "codex"
}

func (rn *runner) ensureClient(ctx context.Context, cwd string) (*codex.Client, error) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.client != nil {
		select {
		case <-rn.client.Done():
			rn.client = nil
			rn.loaded = map[string]bool{}
		default:
			return rn.client, nil
		}
	}
	c, err := codex.Start(rn.codexBinary(), cwd, os.Environ(), os.Stderr)
	if err != nil {
		return nil, err
	}
	if _, err := c.Initialize(ctx, Version); err != nil {
		c.Close()
		return nil, err
	}
	rn.client = c
	return c, nil
}

func (rn *runner) close() {
	rn.mu.Lock()
	c := rn.client
	rn.client = nil
	rn.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (rn *runner) run(r *runState) error {
	ctx := context.Background()
	c, err := rn.ensureClient(ctx, r.cwd)
	if err != nil {
		return err
	}
	sessionCause := "provider-session:" + encodeURIComponent(r.ids.session)
	if !r.hasSession {
		now := store.Now()
		session := func() map[string]any {
			return map[string]any{
				"id": r.ids.session, "driver": "codex", "providerInstanceId": r.instance, "status": "ready",
				"cwd": r.cwd, "model": r.model, "capabilities": codexCapabilities, "extensionStatuses": []any{},
				"createdAt": now, "updatedAt": store.Now(), "lastError": nil,
			}
		}
		if err := r.append(ctx,
			event{cause: sessionCause, typ: "provider-session.attached", driver: "codex", payload: session()},
			event{cause: sessionCause, typ: "provider-session.updated", driver: "codex", payload: session()},
		); err != nil {
			return err
		}
	}
	if _, err := c.Call(ctx, "config/read", map[string]any{"cwd": r.cwd, "includeLayers": false}); err != nil {
		return err
	}
	rn.mu.Lock()
	loaded := r.nativeThread != "" && rn.loaded[r.nativeThread]
	rn.mu.Unlock()
	if !loaded {
		params := map[string]any{
			"cwd": r.cwd, "model": r.model,
			"config": map[string]any{"shell_environment_policy.set.T3CODE_THREAD_ID": r.ids.thread},
		}
		method := "thread/start"
		if r.nativeThread != "" {
			method = "thread/resume"
			params["threadId"] = r.nativeThread
		}
		res, err := c.Call(ctx, method, params)
		if err != nil {
			return err
		}
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(res, &started); err != nil || started.Thread.ID == "" {
			return fmt.Errorf("codex %s returned no thread id", method)
		}
		r.nativeThread = started.Thread.ID
		rn.mu.Lock()
		rn.loaded[r.nativeThread] = true
		rn.mu.Unlock()
		if method == "thread/start" {
			if _, err := c.Call(ctx, "thread/name/set", map[string]any{"name": r.title, "threadId": r.nativeThread}); err != nil {
				return err
			}
		}
	}
	r.startedAt = store.Now()
	if err := r.append(ctx,
		event{typ: "provider-thread.updated", driver: "codex", payload: r.providerThread("active")},
		event{typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload("running", nil, nil)},
		event{typ: "run-attempt.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.attemptPayload("running", nil)},
		event{typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode("running", nil)},
	); err != nil {
		return err
	}
	// The scope's first checkpoint is the workspace before the first run.
	if r.nextCkpt == 0 {
		ref := checkpointRef(r.ids.scope, 0)
		info := &checkpointInfo{ordinal: 0, ref: ref, status: "ready", at: store.Now()}
		if ok, err := captureCheckpoint(r.cwd, ref); err != nil || !ok {
			info.status = "missing"
		}
		r.baseline = info
		r.parent = r.ids.checkpoint(0)
		r.nextCkpt = 1
	}
	if _, err := c.Call(ctx, "config/read", map[string]any{"cwd": r.cwd, "includeLayers": false}); err != nil {
		return err
	}
	notes, cancel := c.Subscribe()
	defer cancel()
	res, err := c.Call(ctx, "turn/start", rn.turnParams(r))
	if err != nil {
		return err
	}
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(res, &turn); err != nil || turn.Turn.ID == "" {
		return errors.New("codex turn/start returned no turn id")
	}
	r.mu.Lock()
	r.turnID = turn.Turn.ID
	r.providerTurn = "provider-turn:provider:codex:native-turn:" + r.turnID
	r.mu.Unlock()
	return rn.follow(ctx, c, r, notes)
}

func (rn *runner) turnParams(r *runState) map[string]any {
	var mode struct {
		RuntimeMode     string
		InteractionMode string
	}
	rn.srv.store.Read(func(st *store.State, _ int64) {
		if t := st.Threads[r.ids.thread]; t != nil {
			mode.RuntimeMode, mode.InteractionMode = t.RuntimeMode, t.InteractionMode
		}
	})
	sandbox := map[string]any{"type": "dangerFullAccess"}
	if mode.RuntimeMode != "full-access" {
		sandbox = map[string]any{"type": "workspaceWrite", "writableRoots": []string{r.cwd}, "networkAccess": false}
	}
	collab := "default"
	if mode.InteractionMode == "plan" {
		collab = "plan"
	}
	return map[string]any{
		"threadId": r.nativeThread, "cwd": r.cwd, "model": r.model,
		"input": []any{map[string]any{"type": "text", "text": r.text}},
		"approvalPolicy": map[string]any{"granular": map[string]any{
			"mcp_elicitations": true, "request_permissions": false, "rules": false,
			"sandbox_approval": false, "skill_approval": false,
		}},
		"approvalsReviewer": "user", "sandboxPolicy": sandbox, "summary": "auto",
		"collaborationMode": map[string]any{"mode": collab, "settings": map[string]any{"model": r.model}},
	}
}

// agentMessage is an assistant message item being streamed.
type agentMessage struct {
	id, node, message, turnItem string
	ordinal                     int
	startedAt                   string
	createdAt                   string
	text                        strings.Builder
	flushed                     int
}

func msToTime(ms int64) string {
	if ms <= 0 {
		return store.Now()
	}
	return time.UnixMilli(ms).UTC().Format(store.TimeFormat)
}

// follow handles codex notifications for the turn until it ends.
func (rn *runner) follow(ctx context.Context, c *codex.Client, r *runState, notes <-chan codex.Notification) error {
	sessionCause := "provider-session:" + encodeURIComponent(r.ids.session)
	items := map[string]*agentMessage{}
	// Deltas are sent as streaming message updates at most this often; an item
	// that completes sooner is sent once, complete (as upstream batches them).
	const flushEvery = 150 * time.Millisecond
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	messagePayload := func(m *agentMessage, streaming bool) map[string]any {
		return map[string]any{
			"createdBy": "agent", "creationSource": "provider", "id": m.message, "threadId": r.ids.thread,
			"runId": r.ids.run, "nodeId": m.node, "role": "assistant", "text": m.text.String(),
			"attachments": []any{}, "streaming": streaming, "createdAt": m.createdAt, "updatedAt": store.Now(),
		}
	}
	itemEvent := func(m *agentMessage, typ string, payload any) event {
		return event{cause: sessionCause, typ: typ, runID: r.ids.run, nodeID: m.node, driver: "codex", payload: payload}
	}
	for {
		select {
		case <-r.stop:
			if _, err := c.Call(ctx, "turn/interrupt", map[string]any{"threadId": r.nativeThread, "turnId": r.turnID}); err != nil {
				return err
			}
			r.stop = nil // wait for turn/completed
		case <-ticker.C:
			for _, m := range items {
				if m.text.Len() > m.flushed {
					m.flushed = m.text.Len()
					if err := r.append(ctx, itemEvent(m, "message.updated", messagePayload(m, true))); err != nil {
						return err
					}
				}
			}
		case n, ok := <-notes:
			if !ok {
				return errors.New("codex app-server exited during the turn")
			}
			if n.ID != nil {
				// A request from codex (approvals, user input). Runs use
				// approval policies that do not ask; anything else is refused.
				_ = c.Respond(n.ID, nil, &codex.RPCError{Code: -32601, Message: "yatt-server does not handle " + n.Method + " yet"})
				continue
			}
			var p struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				ItemID   string `json:"itemId"`
				Delta    string `json:"delta"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
				Item struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
				StartedAtMs   int64 `json:"startedAtMs"`
				CompletedAtMs int64 `json:"completedAtMs"`
			}
			_ = json.Unmarshal(n.Params, &p)
			if p.ThreadID != "" && p.ThreadID != r.nativeThread {
				continue
			}
			switch n.Method {
			case "turn/started":
				r.turnStarted = store.Now()
				if err := r.append(ctx, event{cause: sessionCause, typ: "provider-turn.updated", runID: r.ids.run, nodeID: r.ids.root, driver: "codex",
					payload: r.providerTurnPayload("running", nil)}); err != nil {
					return err
				}
			case "item/started":
				if p.Item.Type == "agentMessage" {
					items[p.Item.ID] = r.newAgentMessage(p.Item.ID, msToTime(p.StartedAtMs))
				}
			case "item/agentMessage/delta":
				m := items[p.ItemID]
				if m == nil {
					m = r.newAgentMessage(p.ItemID, store.Now())
					items[p.ItemID] = m
				}
				m.text.WriteString(p.Delta)
			case "item/completed":
				if p.Item.Type != "agentMessage" {
					continue
				}
				m := items[p.Item.ID]
				if m == nil {
					m = r.newAgentMessage(p.Item.ID, msToTime(p.StartedAtMs))
				}
				delete(items, p.Item.ID)
				m.text.Reset()
				m.text.WriteString(p.Item.Text)
				done := msToTime(p.CompletedAtMs)
				ref := map[string]any{"driver": "codex", "nativeId": m.id, "strength": "strong"}
				if err := r.append(ctx,
					itemEvent(m, "node.updated", map[string]any{
						"id": m.node, "threadId": r.ids.thread, "runId": r.ids.run, "parentNodeId": r.ids.root,
						"rootNodeId": r.ids.root, "kind": "assistant_message", "status": "completed", "countsForRun": false,
						"providerThreadId": r.ids.providerThread, "providerTurnId": r.providerTurn, "nativeItemRef": ref,
						"runtimeRequestId": nil, "checkpointScopeId": nil, "startedAt": m.startedAt, "completedAt": done,
					}),
					itemEvent(m, "message.updated", messagePayload(m, false)),
					itemEvent(m, "turn-item.updated", map[string]any{
						"id": m.turnItem, "threadId": r.ids.thread, "runId": r.ids.run, "nodeId": m.node,
						"providerThreadId": r.ids.providerThread, "providerTurnId": r.providerTurn, "nativeItemRef": ref,
						"parentItemId": nil, "ordinal": m.ordinal, "status": "completed", "title": nil,
						"startedAt": m.startedAt, "completedAt": done, "updatedAt": done, "type": "assistant_message",
						"messageId": m.message, "text": p.Item.Text, "streaming": false,
					}),
				); err != nil {
					return err
				}
			case "turn/completed":
				return rn.finish(ctx, r, p.Turn.Status)
			}
		}
	}
}

func (r *runState) newAgentMessage(itemID, startedAt string) *agentMessage {
	return &agentMessage{
		id: itemID, node: "node:provider:codex:native-item:" + itemID,
		message:  "message:provider:codex:native-item:" + itemID,
		turnItem: "turn-item:provider:codex:native-item:" + itemID,
		ordinal:  r.nextItemOrdinal(), startedAt: startedAt, createdAt: startedAt,
	}
}

// finish records the end of the turn, the run's checkpoint, and the run's
// final status.
func (rn *runner) finish(ctx context.Context, r *runState, turnStatus string) error {
	sessionCause := "provider-session:" + encodeURIComponent(r.ids.session)
	runStatus := "completed"
	switch turnStatus {
	case "interrupted":
		runStatus = "interrupted"
	case "failed":
		runStatus = "failed"
	}
	done := store.Now()
	ended := []event{
		{cause: sessionCause, typ: "provider-turn.updated", runID: r.ids.run, nodeID: r.ids.root, driver: "codex",
			payload: r.providerTurnPayload(runStatus, done)},
		{typ: "run-attempt.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.attemptPayload(runStatus, done)},
	}
	r.mu.Lock()
	request := r.stopRequest
	r.mu.Unlock()
	// A turn the user stopped ends at once (RUN-8); a finished one waits for
	// its checkpoint before it completes.
	interrupted := runStatus == "interrupted"
	if interrupted && request != "" {
		ended = append(ended, event{typ: "turn-item.updated", runID: r.ids.run, nodeID: r.ids.root, payload: map[string]any{
			"id": "turn-item:run:" + encodeURIComponent(r.ids.run) + ":signal:interrupt-result", "threadId": r.ids.thread,
			"runId": r.ids.run, "nodeId": r.ids.root, "providerThreadId": r.ids.providerThread, "providerTurnId": nil,
			"nativeItemRef": nil, "parentItemId": request, "ordinal": r.nextItemOrdinal(), "status": "interrupted",
			"title": "Interrupted", "startedAt": done, "completedAt": done, "updatedAt": done,
			"type": "run_interrupt_result", "message": "Stopped by user",
		}})
	}
	if interrupted {
		ended = append(ended,
			event{typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload(runStatus, done, nil)},
			event{typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode(runStatus, done)},
		)
	} else {
		ended = append(ended,
			event{typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload("waiting", nil, nil)},
			event{typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode("waiting", nil)},
		)
	}
	ended = append(ended, event{typ: "provider-thread.updated", payload: r.providerThread("idle")})
	if err := r.append(ctx, ended...); err != nil {
		return err
	}
	effect := "command:" + encodeURIComponent("command:effect:checkpoint.capture:"+r.ids.run)
	ordinal := r.nextCkpt
	ref := checkpointRef(r.ids.scope, ordinal)
	status := "ready"
	if ok, err := captureCheckpoint(r.cwd, ref); err != nil || !ok {
		status = "missing"
	}
	at := store.Now()
	if interrupted {
		// The run keeps the time it was interrupted.
		at = done
	}
	ckptID := r.ids.checkpoint(ordinal)
	checkpoint := func(n int, runID any, parent any, appRun any, ref, status, at string) map[string]any {
		return map[string]any{
			"id": r.ids.checkpoint(n), "threadId": r.ids.thread, "scopeId": r.ids.scope, "runId": runID,
			"nodeId": r.ids.root, "parentCheckpointId": parent, "ordinalWithinScope": n, "appRunOrdinal": appRun,
			"ref": ref, "status": status, "files": []any{}, "capturedAt": at,
		}
	}
	// Upstream tags the checkpoint item with the driver only on this path.
	checkpointItemDriver := ""
	if interrupted {
		checkpointItemDriver = "codex"
	}
	var evs []event
	if b := r.baseline; b != nil {
		evs = append(evs, event{cause: effect, typ: "checkpoint.captured", nodeID: r.ids.root, driver: "codex",
			payload: checkpoint(0, nil, nil, nil, b.ref, b.status, b.at)})
	}
	evs = append(evs,
		event{cause: effect, typ: "checkpoint.captured", runID: r.ids.run, nodeID: r.ids.root, driver: "codex",
			payload: checkpoint(ordinal, r.ids.run, r.parent, r.ids.ordinal, ref, status, at)},
		event{cause: effect, typ: "turn-item.updated", runID: r.ids.run, nodeID: r.ids.root, driver: checkpointItemDriver, payload: map[string]any{
			"id":       "turn-item:provider:codex:native-item:" + encodeURIComponent("checkpoint:"+ckptID),
			"threadId": r.ids.thread, "runId": r.ids.run, "nodeId": r.ids.root,
			"providerThreadId": r.ids.providerThread, "providerTurnId": nil, "nativeItemRef": nil,
			"parentItemId": nil, "ordinal": r.nextItemOrdinal(), "status": "completed", "title": nil,
			"startedAt": at, "completedAt": at, "updatedAt": at, "type": "checkpoint",
			"checkpointId": ckptID, "scopeId": r.ids.scope, "files": []any{},
		}},
		event{cause: effect, typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload(runStatus, at, ckptID)},
	)
	if !interrupted {
		evs = append(evs, event{cause: effect, typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode(runStatus, at)})
	}
	return r.append(ctx, evs...)
}

// fail ends a run that could not run its turn.
func (rn *runner) fail(r *runState, cause error) {
	ctx := context.Background()
	done := store.Now()
	_ = r.append(ctx,
		event{typ: "run-attempt.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.attemptPayload("failed", done)},
		event{typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload("failed", done, nil)},
		event{typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode("failed", done)},
	)
	_ = cause
}
