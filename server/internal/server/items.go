package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/codex"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Agent items: what codex reports during a turn becomes nodes, messages,
// turn items and runtime requests, as upstream's CodexAdapterV2 builds
// them (spec/behavior/runs.md RUN-14..RUN-19).

// textItem is an assistant message or a reasoning part whose text streams.
type textItem struct {
	reasoning bool
	key       string // the id part of its node and turn item
	nativeID  string // the codex item id
	text      strings.Builder
	flushed   int
}

// turn is what follow knows about the turn it handles.
type turn struct {
	rn           *runner
	c            *codex.Client
	r            *runState
	sessionCause string
	started      map[string]string // native key -> startedAt
	ordinals     map[string]int    // turn item id -> ordinal
	texts        map[string]*textItem
	textOrder    []string
	fileKinds    map[string]map[string]any // file change item -> path -> kind
	responses    chan requestAnswer
}

// requestAnswer is a runtime-request.respond for a request codex waits on.
type requestAnswer struct {
	requestID string
	decision  string
	answers   map[string]any
}

// pendingRequest is a codex request that waits for the user.
type pendingRequest struct {
	run      *runState
	codexID  json.RawMessage
	kind     string            // "command", "file-change", "user_input"
	native   map[string]string // app question id -> codex question id
	answerCh chan requestAnswer
	// A Claude can_use_tool request: its control request id and tool input.
	claudeID    string
	claudeInput map[string]any
}

func itemNodeID(driver, key string) string {
	return "node:provider:" + driver + ":native-item:" + encodeURIComponent(key)
}
func itemTurnItemID(driver, key string) string {
	return "turn-item:provider:" + driver + ":native-item:" + encodeURIComponent(key)
}
func nativeRef(driver, id string) map[string]any {
	return map[string]any{"driver": driver, "nativeId": id, "strength": "strong"}
}

func (t *turn) startedAt(key, at string) string {
	if s, ok := t.started[key]; ok {
		return s
	}
	if at == "" {
		at = store.Now()
	}
	t.started[key] = at
	return at
}

func (t *turn) ordinal(turnItemID string) int {
	if n, ok := t.ordinals[turnItemID]; ok {
		return n
	}
	n := t.r.nextItemOrdinal()
	t.ordinals[turnItemID] = n
	return n
}

func (t *turn) item(nodeID, typ string, payload any) event {
	return event{cause: t.sessionCause, typ: typ, runID: t.r.ids.run, nodeID: nodeID, driver: t.r.driver, payload: payload}
}

func (t *turn) node(id, kind, status, startedAt string, completedAt any, nativeID string, requestID any, parent string) map[string]any {
	if parent == "" {
		parent = t.r.ids.root
	}
	return map[string]any{
		"id": id, "threadId": t.r.ids.thread, "runId": t.r.ids.run, "parentNodeId": parent,
		"rootNodeId": t.r.ids.root, "kind": kind, "status": status, "countsForRun": false,
		"providerThreadId": t.r.ids.providerThread, "providerTurnId": t.r.providerTurn,
		"nativeItemRef": nativeRef(t.r.driver, nativeID), "runtimeRequestId": requestID, "checkpointScopeId": nil,
		"startedAt": startedAt, "completedAt": completedAt,
	}
}

func (t *turn) turnItem(id, nodeID, typ, status, startedAt string, completedAt any, nativeID string, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": id, "threadId": t.r.ids.thread, "runId": t.r.ids.run, "nodeId": nodeID,
		"providerThreadId": t.r.ids.providerThread, "providerTurnId": t.r.providerTurn,
		"nativeItemRef": nativeRef(t.r.driver, nativeID), "parentItemId": nil, "ordinal": t.ordinal(id),
		"status": status, "title": nil, "startedAt": startedAt, "completedAt": completedAt,
		"updatedAt": store.Now(), "type": typ,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// --- streaming text ---------------------------------------------------------------

func (t *turn) text(key, nativeID string, reasoning bool) *textItem {
	if x, ok := t.texts[key]; ok {
		return x
	}
	x := &textItem{reasoning: reasoning, key: key, nativeID: nativeID}
	t.texts[key] = x
	t.textOrder = append(t.textOrder, key)
	return x
}

// textEvents are the events for a text item's current state.
func (t *turn) textEvents(x *textItem, completed bool) []event {
	now := store.Now()
	var completedAt any
	status := "running"
	if completed {
		completedAt, status = now, "completed"
	}
	startedAt := t.startedAt(x.key, "")
	nodeID, itemID := itemNodeID(t.r.driver, x.key), itemTurnItemID(t.r.driver, x.key)
	kind, typ := "assistant_message", "assistant_message"
	if x.reasoning {
		kind, typ = "reasoning", "reasoning"
	}
	messageID := "message:provider:" + t.r.driver + ":native-item:" + encodeURIComponent(x.key)
	extra := map[string]any{"text": x.text.String(), "streaming": !completed}
	if !x.reasoning {
		extra["messageId"] = messageID
	}
	if x.reasoning && t.r.driver == claudeDriver {
		// Claude thinking is a turn item of the root node, without a node.
		item := t.turnItem(itemID, t.r.ids.root, typ, status, startedAt, completedAt, x.nativeID, extra)
		item["title"] = "Thinking"
		return []event{t.item(t.r.ids.root, "turn-item.updated", item)}
	}
	evs := []event{t.item(nodeID, "node.updated", t.node(nodeID, kind, status, startedAt, completedAt, x.nativeID, nil, ""))}
	if completed && !x.reasoning {
		evs = append(evs, t.item(nodeID, "message.updated", map[string]any{
			"createdBy": "agent", "creationSource": "provider", "id": messageID, "threadId": t.r.ids.thread,
			"runId": t.r.ids.run, "nodeId": nodeID, "role": "assistant", "text": x.text.String(),
			"attachments": []any{}, "streaming": false, "createdAt": startedAt, "updatedAt": now,
		}))
	}
	return append(evs, t.item(nodeID, "turn-item.updated", t.turnItem(itemID, nodeID, typ, status, startedAt, completedAt, x.nativeID, extra)))
}

// flush sends streaming updates for text that grew since the last flush.
func (t *turn) flush(ctx context.Context) error {
	for _, key := range t.textOrder {
		x := t.texts[key]
		if x == nil || x.text.Len() <= x.flushed {
			continue
		}
		x.flushed = x.text.Len()
		if err := t.r.append(ctx, t.textEvents(x, false)...); err != nil {
			return err
		}
	}
	return nil
}

// complete ends a text item with its final text; an empty reasoning part
// that never streamed is dropped.
func (t *turn) complete(ctx context.Context, x *textItem, final string) error {
	delete(t.texts, x.key)
	if final != "" {
		x.text.Reset()
		x.text.WriteString(final)
	}
	if x.reasoning && x.text.Len() == 0 {
		return nil
	}
	return t.r.append(ctx, t.textEvents(x, true)...)
}

func reasoningKey(turnID, itemID, stream string, index int) string {
	b, _ := json.Marshal([]any{turnID, itemID, stream, index})
	return string(b)
}

// --- tool items ------------------------------------------------------------------

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Status           string          `json:"status"`
	Command          string          `json:"command"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Summary          []string        `json:"summary"`
	Content          []string        `json:"content"`
	Changes          []codexChange   `json:"changes"`
	Raw              json.RawMessage `json:"-"`
}

type codexChange struct {
	Path string         `json:"path"`
	Diff string         `json:"diff"`
	Kind map[string]any `json:"kind"`
}

// itemStatus maps a codex item status to node and turn item statuses.
func itemStatus(s string) (status string, completed bool) {
	switch s {
	case "completed":
		return "completed", true
	case "failed":
		return "failed", true
	case "declined":
		return "cancelled", true
	}
	return "running", false
}

func (t *turn) commandEvents(it codexItem, startedAtMs int64) []event {
	status, completed := itemStatus(it.Status)
	var completedAt any
	if completed {
		completedAt = store.Now()
	}
	startedAt := t.startedAt(it.ID, msToTime(startedAtMs))
	nodeID, itemID := itemNodeID(t.r.driver, it.ID), itemTurnItemID(t.r.driver, it.ID)
	extra := map[string]any{"input": it.Command, "completionWake": "always"}
	if it.AggregatedOutput != nil {
		extra["output"] = *it.AggregatedOutput
	}
	if it.ExitCode != nil {
		extra["exitCode"] = *it.ExitCode
	}
	return []event{
		t.item(nodeID, "node.updated", t.node(nodeID, "tool_call", status, startedAt, completedAt, it.ID, nil, "")),
		t.item(nodeID, "turn-item.updated", t.turnItem(itemID, nodeID, "command_execution", status, startedAt, completedAt, it.ID, extra)),
	}
}

func (t *turn) fileChangeEvents(it codexItem, startedAtMs int64) []event {
	if len(it.Changes) == 0 {
		return nil
	}
	status, completed := itemStatus(it.Status)
	var completedAt any
	if completed {
		completedAt = store.Now()
	}
	startedAt := t.startedAt(it.ID, msToTime(startedAtMs))
	nodeID, itemID := itemNodeID(t.r.driver, it.ID), itemTurnItemID(t.r.driver, it.ID)
	changes := []any{}
	for _, ch := range it.Changes {
		op, _ := ch.Kind["type"].(string)
		if move, _ := ch.Kind["move_path"].(string); op == "update" && move != "" {
			changes = append(changes, map[string]any{"operation": op, "path": move, "oldPath": ch.Path})
		} else {
			changes = append(changes, map[string]any{"operation": op, "path": ch.Path})
		}
	}
	extra := map[string]any{"fileName": it.Changes[0].Path, "diffStr": it.Changes[0].Diff, "changes": changes}
	return []event{
		t.item(nodeID, "node.updated", t.node(nodeID, "tool_call", status, startedAt, completedAt, it.ID, nil, "")),
		t.item(nodeID, "turn-item.updated", t.turnItem(itemID, nodeID, "file_change", status, startedAt, completedAt, it.ID, extra)),
	}
}

// fileChangePrompt describes a file change approval as upstream does.
func fileChangePrompt(reason, grantRoot string, kinds map[string]any) any {
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	if len(kinds) > 0 {
		paths := make([]string, 0, len(kinds))
		for p := range kinds {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		var lines []string
		for i, p := range paths {
			if i == 20 {
				break
			}
			k, _ := kinds[p].(map[string]any)
			typ, _ := k["type"].(string)
			if move, _ := k["move_path"].(string); typ == "update" && move != "" {
				lines = append(lines, typ+" "+p+" -> "+move)
			} else {
				lines = append(lines, typ+" "+p)
			}
		}
		if rest := len(paths) - len(lines); rest > 0 {
			return strings.Join(lines, "\n") + fmt.Sprintf("\n+%d more", rest)
		}
		return strings.Join(lines, "\n")
	}
	if g := strings.TrimSpace(grantRoot); g != "" {
		return g
	}
	return nil
}

// --- runtime requests -------------------------------------------------------------

func (t *turn) requestID(nativeRequestID string) (string, error) {
	u, err := newUUID()
	if err != nil {
		return "", err
	}
	return "runtime-request:provider:" + t.r.driver + ":provider-turn:" + encodeURIComponent(t.r.providerTurn) +
		":native-request:" + encodeURIComponent(nativeRequestID) + ":" + u, nil
}

func (t *turn) runtimeRequest(id, nodeID, nativeRequestID, kind, createdAt string) map[string]any {
	return map[string]any{
		"id": id, "nodeId": nodeID, "providerTurnId": t.r.providerTurn,
		"nativeRequestRef": nativeRef(t.r.driver, nativeRequestID), "kind": kind, "status": "pending",
		"responseCapability": map[string]any{"type": "live", "providerSessionId": t.r.ids.session},
		"createdAt":          createdAt, "resolvedAt": nil,
	}
}

// approval records a codex approval request and waits for the user.
func (t *turn) approval(ctx context.Context, n codex.Notification, kind string) error {
	var p struct {
		ItemID     string  `json:"itemId"`
		ApprovalID *string `json:"approvalId"`
		Reason     *string `json:"reason"`
		Command    *string `json:"command"`
		GrantRoot  *string `json:"grantRoot"`
	}
	_ = json.Unmarshal(n.Params, &p)
	if t.rn.allowed(t.r.nativeThread, kind) {
		return t.c.Respond(n.ID, map[string]any{"decision": "acceptForSession"}, nil)
	}
	native := p.ItemID
	if kind == "command" && p.ApprovalID != nil {
		native = *p.ApprovalID
	}
	var prompt any
	switch kind {
	case "command":
		if p.Reason != nil {
			prompt = *p.Reason
		} else if p.Command != nil {
			prompt = *p.Command
		}
	case "file-change":
		prompt = fileChangePrompt(deref(p.Reason), deref(p.GrantRoot), t.fileKinds[p.ItemID])
	}
	id, err := t.requestID(native)
	if err != nil {
		return err
	}
	now := store.Now()
	nodeID := "node:runtime-request:" + encodeURIComponent(id)
	itemID := "turn-item:runtime-request:" + encodeURIComponent(id)
	extra := map[string]any{"requestId": id, "requestKind": kind}
	if prompt != nil {
		extra["prompt"] = prompt
	}
	t.ordinal(itemID)
	item := t.turnItem(itemID, nodeID, "approval_request", "waiting", now, nil, p.ItemID, extra)
	item["updatedAt"] = now
	if err := t.r.append(ctx,
		t.item(nodeID, "node.updated", t.node(nodeID, "approval_request", "waiting", now, nil, p.ItemID, id, itemNodeID(t.r.driver, p.ItemID))),
		t.item(nodeID, "turn-item.updated", item),
		t.item(nodeID, "runtime-request.updated", t.runtimeRequest(id, nodeID, native, kind, now)),
	); err != nil {
		return err
	}
	t.rn.waitFor(id, &pendingRequest{run: t.r, codexID: n.ID, kind: kind, answerCh: t.responses})
	return nil
}

// userInput records a codex question for the user.
func (t *turn) userInput(ctx context.Context, n codex.Notification) error {
	var p struct {
		ItemID    string `json:"itemId"`
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(n.Params, &p)
	id, err := t.requestID(p.ItemID)
	if err != nil {
		return err
	}
	now := store.Now()
	nonEmpty := func(s, fallback string) string {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	}
	native := map[string]string{}
	questions := []any{}
	for i, q := range p.Questions {
		qid := nonEmpty(q.ID, fmt.Sprintf("question-%d", i+1))
		native[qid] = q.ID
		options := []any{}
		for j, o := range q.Options {
			options = append(options, map[string]any{
				"label":       nonEmpty(o.Label, fmt.Sprintf("Option %d", j+1)),
				"description": nonEmpty(o.Description, o.Label),
			})
		}
		questions = append(questions, map[string]any{
			"id": qid, "header": nonEmpty(q.Header, "Question"),
			"question": nonEmpty(q.Question, "Choose an answer."), "options": options,
		})
	}
	nodeID, itemID := itemNodeID(t.r.driver, p.ItemID), itemTurnItemID(t.r.driver, p.ItemID)
	item := t.turnItem(itemID, nodeID, "user_input_request", "waiting", now, nil, p.ItemID,
		map[string]any{"requestId": id, "questions": questions})
	item["updatedAt"] = now
	if err := t.r.append(ctx,
		t.item(nodeID, "node.updated", t.node(nodeID, "user_input_request", "waiting", now, nil, p.ItemID, id, "")),
		t.item(nodeID, "turn-item.updated", item),
		t.item(nodeID, "runtime-request.updated", t.runtimeRequest(id, nodeID, p.ItemID, "user_input", now)),
	); err != nil {
		return err
	}
	t.rn.waitFor(id, &pendingRequest{run: t.r, codexID: n.ID, kind: "user_input", native: native, answerCh: t.responses})
	return nil
}

// answer sends the user's response to codex.
func (t *turn) answer(a requestAnswer) error {
	pr := t.rn.takePending(a.requestID)
	if pr == nil {
		return nil
	}
	if pr.kind == "user_input" {
		out := map[string]any{}
		for qid, v := range a.answers {
			native, ok := pr.native[qid]
			if !ok {
				continue
			}
			out[native] = map[string]any{"answers": answerStrings(v)}
		}
		return t.c.Respond(pr.codexID, map[string]any{"answers": out}, nil)
	}
	decision := a.decision
	switch decision {
	case "acceptForSession", "acceptAlways":
		t.rn.allow(t.r.nativeThread, pr.kind)
		decision = "acceptForSession"
	case "":
		decision = "decline"
	}
	return t.c.Respond(pr.codexID, map[string]any{"decision": decision}, nil)
}

func answerStrings(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = fmt.Sprint(e)
		}
		return out
	case string:
		return []string{x}
	case nil:
		return []string{}
	case float64, bool:
		return []string{fmt.Sprint(x)}
	}
	b, _ := json.Marshal(v)
	return []string{string(b)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- the turn loop ------------------------------------------------------------------

func msToTime(ms int64) string {
	if ms <= 0 {
		return store.Now()
	}
	return time.UnixMilli(ms).UTC().Format(store.TimeFormat)
}

// follow handles codex notifications and requests for the turn until it ends.
func (rn *runner) follow(ctx context.Context, c *codex.Client, r *runState, notes <-chan codex.Notification) error {
	t := &turn{
		rn: rn, c: c, r: r, sessionCause: "provider-session:" + encodeURIComponent(r.ids.session),
		started: map[string]string{}, ordinals: map[string]int{}, texts: map[string]*textItem{},
		fileKinds: map[string]map[string]any{}, responses: make(chan requestAnswer, 4),
	}
	r.mu.Lock()
	r.answers = t.responses
	r.mu.Unlock()
	// Text deltas are sent as streaming updates at most this often; an item
	// that completes sooner is sent once, complete (as upstream coalesces them).
	const flushEvery = 150 * time.Millisecond
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			if _, err := c.Call(ctx, "turn/interrupt", map[string]any{"threadId": r.nativeThread, "turnId": r.turnID}); err != nil {
				return err
			}
			r.stop = nil // wait for turn/completed
		case <-ticker.C:
			if err := t.flush(ctx); err != nil {
				return err
			}
		case a := <-t.responses:
			if err := t.answer(a); err != nil {
				return err
			}
		case n, ok := <-notes:
			if !ok {
				return errors.New("codex app-server exited during the turn")
			}
			if n.ID != nil {
				var err error
				switch n.Method {
				case "item/commandExecution/requestApproval", "execCommandApproval":
					err = t.approval(ctx, n, "command")
				case "item/fileChange/requestApproval", "applyPatchApproval":
					err = t.approval(ctx, n, "file-change")
				case "item/tool/requestUserInput":
					err = t.userInput(ctx, n)
				default:
					err = c.Respond(n.ID, nil, &codex.RPCError{Code: -32601, Message: "yatt-server does not handle " + n.Method + " yet"})
				}
				if err != nil {
					return err
				}
				continue
			}
			done, err := t.notification(ctx, n)
			if err != nil || done {
				return err
			}
		}
	}
}

// notification handles one codex notification; done reports the turn's end.
func (t *turn) notification(ctx context.Context, n codex.Notification) (bool, error) {
	r := t.r
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
		Index    int    `json:"summaryIndex"`
		Content  int    `json:"contentIndex"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		Item          codexItem `json:"item"`
		StartedAtMs   int64     `json:"startedAtMs"`
		CompletedAtMs int64     `json:"completedAtMs"`
	}
	_ = json.Unmarshal(n.Params, &p)
	if p.ThreadID != "" && p.ThreadID != r.nativeThread {
		return false, nil
	}
	switch n.Method {
	case "turn/started":
		r.turnStarted = store.Now()
		return false, r.append(ctx, event{cause: t.sessionCause, typ: "provider-turn.updated", runID: r.ids.run, nodeID: r.ids.root, driver: r.driver,
			payload: r.providerTurnPayload("running", nil)})
	case "item/started":
		it := p.Item
		if it.Type != "reasoning" {
			t.startedAt(it.ID, msToTime(p.StartedAtMs))
		}
		switch it.Type {
		case "agentMessage":
			t.text(it.ID, it.ID, false)
		case "fileChange":
			kinds := map[string]any{}
			for _, ch := range it.Changes {
				kinds[ch.Path] = ch.Kind
			}
			t.fileKinds[it.ID] = kinds
		case "commandExecution":
			return false, r.append(ctx, t.commandEvents(it, p.StartedAtMs)...)
		}
	case "item/agentMessage/delta":
		t.text(p.ItemID, p.ItemID, false).text.WriteString(p.Delta)
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if p.Delta == "" {
			return false, nil
		}
		stream, index := "summary", p.Index
		if n.Method == "item/reasoning/textDelta" {
			stream, index = "content", p.Content
		}
		key := reasoningKey(p.TurnID, p.ItemID, stream, index)
		t.startedAt(key, "")
		t.ordinal(itemTurnItemID(t.r.driver, key))
		t.text(key, p.ItemID, true).text.WriteString(p.Delta)
	case "item/completed":
		it := p.Item
		switch it.Type {
		case "agentMessage":
			return false, t.complete(ctx, t.text(it.ID, it.ID, false), it.Text)
		case "reasoning":
			for _, stream := range []string{"summary", "content"} {
				parts := it.Summary
				if stream == "content" {
					parts = it.Content
				}
				for i := range parts {
					key := reasoningKey(p.TurnID, it.ID, stream, i)
					t.startedAt(key, "")
					t.text(key, it.ID, true)
				}
			}
			for _, key := range append([]string(nil), t.textOrder...) {
				x := t.texts[key]
				if x == nil || !x.reasoning || x.nativeID != it.ID {
					continue
				}
				var parts []string
				var stream string
				var index int
				var raw []any
				_ = json.Unmarshal([]byte(key), &raw)
				if len(raw) == 4 {
					stream, _ = raw[2].(string)
					f, _ := raw[3].(float64)
					index = int(f)
				}
				if stream == "content" {
					parts = it.Content
				} else {
					parts = it.Summary
				}
				final := ""
				if index < len(parts) {
					final = parts[index]
				}
				if err := t.complete(ctx, x, final); err != nil {
					return false, err
				}
			}
		case "commandExecution":
			return false, r.append(ctx, t.commandEvents(it, p.StartedAtMs)...)
		case "fileChange":
			delete(t.fileKinds, it.ID)
			if evs := t.fileChangeEvents(it, p.StartedAtMs); evs != nil {
				return false, r.append(ctx, evs...)
			}
		}
	case "turn/completed":
		if err := t.flush(ctx); err != nil {
			return true, err
		}
		return true, t.rn.finish(ctx, r, p.Turn.Status)
	}
	return false, nil
}

// --- wire form ---------------------------------------------------------------------

var (
	failureHint = regexp.MustCompile(`(?i)enoent|no such|no files found|not found|commandnotfound|not recognized|cannot (?:find path|be found)|exit`)
	failureText = regexp.MustCompile(`(?i)file not found|no files found|enoent|no such file|commandnotfoundexception|command not found|is not recognized as the name of a cmdlet|a parameter cannot be found that matches parameter name`)
	exitCodeRe1 = regexp.MustCompile(`(?i)exit(?:ed)? with exit code\s+[1-9]\d*`)
	exitCodeRe2 = regexp.MustCompile(`(?i)exit code\s*[:\s]\s*[1-9]\d*\b`)
)

func outputIndicatesFailure(text string) bool {
	if len(text) > 32768 {
		text = text[:32768]
	}
	if !failureHint.MatchString(text) {
		return false
	}
	lower := strings.ToLower(text)
	return failureText.MatchString(text) ||
		(strings.Contains(lower, "cannot find path") && strings.Contains(lower, "because it does not exist")) ||
		(strings.Contains(lower, "is not recognized") && strings.Contains(lower, "the term '")) ||
		exitCodeRe1.MatchString(text) || exitCodeRe2.MatchString(text)
}

// wireTurnItem is a turn item as clients receive it: command output and
// file diffs stay on the server (upstream projectTurnItemForWire).
func wireTurnItem(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	switch m["type"] {
	case "command_execution":
		output, hasOutput := m["output"].(string)
		delete(m, "output")
		failed := false
		if code, ok := m["exitCode"].(float64); ok {
			failed = code != 0
		} else if b, _ := m["outputIndicatesFailure"].(bool); b {
			failed = true
		} else if hasOutput {
			failed = outputIndicatesFailure(output)
		}
		if failed {
			m["outputIndicatesFailure"] = true
		}
		if strings.TrimSpace(output) != "" {
			m["outputOmitted"] = true
		}
	case "file_change":
		diff, _ := m["diffStr"].(string)
		delete(m, "diffStr")
		delete(m, "oldStr")
		delete(m, "newStr")
		if m["status"] == "failed" && strings.TrimSpace(diff) != "" {
			m["diffStr"] = truncateDetail(diff)
		}
	case "dynamic_tool":
		output, ok := m["output"]
		if !ok {
			return raw
		}
		delete(m, "output")
		if str, isStr := output.(string); !isStr || strings.TrimSpace(str) != "" {
			m["outputOmitted"] = true
		}
	case "checkpoint":
		if !capCheckpointFiles(m) {
			return raw
		}
	default:
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// truncateDetail bounds a string to 32 KiB, marker included, cutting at a
// UTF-8 boundary (upstream WireProjection truncateDetail).
func truncateDetail(s string) string {
	const max = 32768
	const marker = "\n… output truncated for transport"
	if len(s) <= max {
		return s
	}
	cut := max - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// --- runtime-request.respond ---------------------------------------------------------

type respondCommand struct {
	CommandID string         `json:"commandId"`
	ThreadID  string         `json:"threadId"`
	RequestID string         `json:"requestId"`
	Decision  *string        `json:"decision"`
	Answers   map[string]any `json:"answers"`
	Attach    map[string]any `json:"attachmentsByQuestionId"`
}

// respondRuntimeRequest resolves a pending request and sends the answer
// to codex (RUN-17).
func (s *Server) respondRuntimeRequest(ctx context.Context, payload json.RawMessage) (any, error) {
	var cmd respondCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	tc := threadCommand{Type: "runtime-request.respond", CommandID: cmd.CommandID, ThreadID: cmd.ThreadID}
	answers := cmd.Answers
	events, err := s.store.DecideMany(ctx, cmd.CommandID, func(st *store.State) ([]store.Draft, error) {
		t, ok := st.Threads[cmd.ThreadID]
		if !ok {
			return nil, threadNotFound(tc)
		}
		req := t.Proj.Get("runtimeRequests", cmd.RequestID)
		if req == nil {
			return nil, dispatchError(tc, "Runtime request "+cmd.RequestID+" was not found.")
		}
		if status := req.String("status"); status != "pending" {
			return nil, dispatchError(tc, "Runtime request "+cmd.RequestID+" is "+status+".")
		}
		var resolved map[string]any
		_ = json.Unmarshal(req.Payload, &resolved)
		now := store.Now()
		resolved["status"], resolved["resolvedAt"] = "resolved", now
		if cmd.Decision != nil {
			resolved["decision"] = *cmd.Decision
		}
		if req.String("kind") == "user_input" && answers != nil {
			flat := map[string]any{}
			for k, v := range answers {
				if m, ok := v.(map[string]any); ok {
					if a, ok := m["answers"]; ok {
						v = a
					}
				}
				flat[k] = v
			}
			answers = flat
		}
		if answers != nil {
			resolved["answers"] = answers
		}
		status := "completed"
		if cmd.Decision != nil && (*cmd.Decision == "decline" || *cmd.Decision == "cancel") {
			status = "cancelled"
		}
		cause := "command:" + cmd.CommandID
		nodeID := req.String("nodeId")
		node := t.Proj.Get("nodes", nodeID)
		runID := ""
		if node != nil {
			runID = node.String("runId")
		}
		instance, _ := providerInstanceID(t).(string)
		mk := func(typ, nodeID string, payload any) (store.Draft, error) {
			d, err := envelope(t.ID, cause, typ, payload)
			if err != nil {
				return d, err
			}
			env := d.Data.(*store.Envelope)
			env.RunID, env.NodeID, env.Driver, env.ProviderInstanceID = runID, nodeID, driverOf(instance), instance
			return d, nil
		}
		var drafts []store.Draft
		d, err := mk("runtime-request.updated", nodeID, resolved)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
		if node != nil {
			var n map[string]any
			_ = json.Unmarshal(node.Payload, &n)
			n["status"], n["completedAt"] = status, now
			if d, err = mk("node.updated", nodeID, n); err != nil {
				return nil, err
			}
			drafts = append(drafts, d)
		}
		var item *store.Entity
		for _, it := range t.Proj.List("turnItems") {
			if it.String("requestId") == cmd.RequestID {
				item = it
			}
		}
		if item != nil && driverOf(instance) == claudeDriver && status == "cancelled" && item.String("type") == "approval_request" {
			// Upstream leaves a declined Claude approval item waiting.
			item = nil
		}
		if item != nil {
			var m map[string]any
			_ = json.Unmarshal(item.Payload, &m)
			if m["type"] == "user_input_request" && answers != nil {
				texts := map[string]any{}
				if qs, ok := m["questions"].([]any); ok {
					for _, q := range qs {
						if qm, ok := q.(map[string]any); ok {
							id, _ := qm["id"].(string)
							texts[id] = qm["question"]
						}
					}
				}
				attach := cmd.Attach
				if attach == nil {
					attach = map[string]any{}
				}
				m["questionAnswer"] = map[string]any{
					"requestId": cmd.RequestID, "answers": answers,
					"attachmentsByQuestionId": attach, "questionTextById": texts,
				}
			}
			m["status"], m["completedAt"], m["updatedAt"] = status, now, now
			itemNode, _ := m["nodeId"].(string)
			if d, err = mk("turn-item.updated", itemNode, m); err != nil {
				return nil, err
			}
			drafts = append(drafts, d)
		}
		return drafts, nil
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return nil, err
	}
	if err == nil {
		if pr := s.runs.pendingRequest(cmd.RequestID); pr != nil {
			a := requestAnswer{requestID: cmd.RequestID, answers: answers}
			if cmd.Decision != nil {
				a.decision = *cmd.Decision
			}
			select {
			case pr.answerCh <- a:
			default:
			}
		}
	}
	return map[string]any{"sequence": events[len(events)-1].Sequence}, nil
}

// maxWireCheckpointFiles caps the files a checkpoint sends to clients
// (upstream WireProjection MAX_WIRE_CHECKPOINT_FILES).
const maxWireCheckpointFiles = 200

// capCheckpointFiles keeps the first 200 files of a checkpoint or checkpoint
// turn item and adds filesTotal for the whole list. It reports whether m
// changed.
func capCheckpointFiles(m map[string]any) bool {
	files, _ := m["files"].([]any)
	if len(files) <= maxWireCheckpointFiles || m["filesTotal"] != nil {
		return false
	}
	var additions, deletions float64
	for _, f := range files {
		if fm, ok := f.(map[string]any); ok {
			a, _ := fm["additions"].(float64)
			d, _ := fm["deletions"].(float64)
			additions += a
			deletions += d
		}
	}
	m["files"] = files[:maxWireCheckpointFiles]
	m["filesTotal"] = map[string]any{"count": len(files), "additions": additions, "deletions": deletions}
	return true
}

// wireCheckpoint is a checkpoint payload as clients receive it.
func wireCheckpoint(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || !capCheckpointFiles(m) {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
