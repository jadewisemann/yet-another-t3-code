package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/claude"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Claude runs: a run whose provider instance is claudeAgent drives the
// `claude` CLI in stream-json mode, as upstream's ClaudeAdapterV2 does
// through the Claude Agent SDK. Each thread keeps one CLI process (its
// provider session) across runs; a turn is one user message and ends with
// the CLI's result message (spec/behavior/runs.md RUN-21..RUN-24).

const claudeDriver = "claudeAgent"

//go:embed claude-capabilities.json
var claudeCapabilities json.RawMessage

// claudeSession is a thread's running CLI.
type claudeSession struct {
	p     *claude.Process
	model string
	mode  string
}

func (rn *runner) claudeBinary() string {
	if b := os.Getenv("YATT_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

// claudePermissionMode is the CLI permission mode for the thread's modes.
func claudePermissionMode(runtimeMode, interactionMode string) string {
	switch {
	case interactionMode == "plan":
		return "plan"
	case runtimeMode == "full-access":
		return "bypassPermissions"
	}
	return "default"
}

// ensureClaude returns the thread's CLI, starting it when it is not
// running or runs with another model or permission mode. A thread with a
// native session resumes it; a new one gets its session id from us.
func (rn *runner) ensureClaude(ctx context.Context, r *runState, mode string) (*claude.Process, error) {
	rn.mu.Lock()
	s := rn.claudes[r.ids.thread]
	rn.mu.Unlock()
	if s != nil {
		select {
		case <-s.p.Done():
			s = nil
		default:
			if s.model == r.model && s.mode == mode {
				return s.p, nil
			}
			s.p.Close()
			s = nil
		}
	}
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--thinking=adaptive", "--thinking-display=summarized", "--effort=medium",
		"--permission-prompt-tool=stdio", "--tools=default", "--permission-mode=" + mode,
		"--include-partial-messages",
	}
	if r.model != "" {
		args = append(args, "--model="+r.model)
	}
	if mode == "bypassPermissions" {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if r.cwd != "" {
		args = append(args, "--add-dir="+r.cwd)
	}
	if r.nativeThread != "" {
		args = append(args, "--resume="+r.nativeThread)
	} else {
		u, err := newUUID()
		if err != nil {
			return nil, err
		}
		r.nativeThread = u
		args = append(args, "--session-id="+u)
	}
	p, err := claude.Start(rn.claudeBinary(), args, r.cwd, claudeEnvironment(), os.Stderr)
	if err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := p.Control(initCtx, map[string]any{"subtype": "initialize"}); err != nil {
		p.Close()
		return nil, fmt.Errorf("claude did not start: %w", err)
	}
	rn.mu.Lock()
	rn.claudes[r.ids.thread] = &claudeSession{p: p, model: r.model, mode: mode}
	rn.mu.Unlock()
	return p, nil
}

// claudeEnvironment is the server's environment without the variables a
// Claude Code session that launched the server exports: a CLI started with
// them would act as part of that session (upstream ClaudeHome).
func claudeEnvironment() []string {
	drop := map[string]bool{
		"CLAUDECODE": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_HOST_SESSION_ID": true,
		"CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
		"CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
		"CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_DESKTOP_APP_VERSION": true, "CLAUDE_EFFORT": true,
		"CLAUDE_PID": true, "CLAUDE_AGENT_SDK_VERSION": true, "CLAUDE_CODE_ENTRYPOINT": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	return env
}

func (rn *runner) closeClaudes() {
	rn.mu.Lock()
	sessions := rn.claudes
	rn.claudes = map[string]*claudeSession{}
	rn.mu.Unlock()
	for _, s := range sessions {
		s.p.Close()
	}
}

// claudeContextWindow is the context size upstream reports for a model.
func claudeContextWindow(model string) int {
	for _, m := range []string{"opus-5", "sonnet-5", "fable-5", "opus-4-8", "opus-4-7", "opus-4-6", "sonnet-4-6"} {
		if strings.Contains(model, m) {
			return 1000000
		}
	}
	return 200000
}

// claudeAutoCompactReserve is what upstream subtracts from the auto-compact
// window to get the threshold it reports.
const claudeAutoCompactReserve = 33000

// claudeAutoCompactThreshold is the token count at which the CLI compacts,
// or 0 when no auto-compact window is configured or it is not below the
// model's window. Like upstream's resolveAutoCompactWindow it reads, in
// order, the project's .claude/settings.local.json and .claude/settings.json,
// the user's Claude settings.json and then the environment.
func claudeAutoCompactThreshold(cwd, model string, maxTokens int) int {
	var files []map[string]any
	paths := []string{}
	if cwd != "" {
		paths = append(paths, filepath.Join(cwd, ".claude", "settings.local.json"), filepath.Join(cwd, ".claude", "settings.json"))
	}
	paths = append(paths, filepath.Join(claudeConfigDir(), "settings.json"))
	for _, p := range paths {
		var m map[string]any
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &m) == nil {
			files = append(files, m)
		}
	}
	env := func(name string) (string, bool) {
		for _, f := range files {
			if e, ok := f["env"].(map[string]any); ok {
				if v, ok := e[name].(string); ok {
					return v, true
				}
			}
		}
		return os.LookupEnv(name)
	}
	truthy := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	for _, name := range []string{"DISABLE_COMPACT", "DISABLE_AUTO_COMPACT"} {
		if v, ok := env(name); ok && truthy(v) {
			return 0
		}
	}
	for _, f := range files {
		if on, ok := f["autoCompactEnabled"].(bool); ok {
			if !on {
				return 0
			}
			break
		}
	}
	valid := func(v any) int {
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case string:
			if _, err := fmt.Sscan(strings.TrimSpace(x), &n); err != nil {
				return 0
			}
		default:
			return 0
		}
		if n != float64(int(n)) || n < 100000 || n > 1000000 {
			return 0
		}
		return int(n)
	}
	window := 0
	if v, ok := env("CLAUDE_CODE_AUTO_COMPACT_WINDOW"); ok {
		window = valid(v)
	}
	if window == 0 {
		name := strings.TrimSuffix(strings.TrimSuffix(model, "[1m]"), "[1M]")
	files:
		for _, f := range files {
			if ms, ok := f["modelSettings"].(map[string]any); ok {
				if s, ok := ms[name].(map[string]any); ok {
					if s["autoCompactWindow"] == "auto" {
						break files
					}
					if window = valid(s["autoCompactWindow"]); window != 0 {
						break
					}
				}
			}
			if window = valid(f["autoCompactWindow"]); window != 0 {
				break
			}
		}
	}
	if window == 0 || window >= maxTokens {
		return 0
	}
	return window - claudeAutoCompactReserve
}

// claudeConfigDir is where the CLI keeps its user settings.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func (rn *runner) runClaude(r *runState) error {
	ctx := context.Background()
	var mode struct{ runtime, interaction string }
	rn.srv.store.Read(func(st *store.State, _ int64) {
		if t := st.Threads[r.ids.thread]; t != nil {
			mode.runtime, mode.interaction = t.RuntimeMode, t.InteractionMode
		}
	})
	permissionMode := claudePermissionMode(mode.runtime, mode.interaction)
	sessionCause := "provider-session:" + encodeURIComponent(r.ids.session)
	if r.session == nil {
		now := store.Now()
		session := func() map[string]any {
			return map[string]any{
				"id": r.ids.session, "driver": r.driver, "providerInstanceId": r.instance, "status": "ready",
				"cwd": r.cwd, "model": r.model, "capabilities": claudeCapabilities, "extensionStatuses": []any{},
				"createdAt": now, "updatedAt": store.Now(), "lastError": nil,
			}
		}
		if err := r.append(ctx,
			event{cause: sessionCause, typ: "provider-session.attached", driver: r.driver, payload: session()},
			event{cause: sessionCause, typ: "provider-session.updated", driver: r.driver, payload: session()},
		); err != nil {
			return err
		}
	} else if err := r.append(ctx, event{cause: sessionCause, typ: "provider-session.updated", driver: r.driver, payload: r.session.Payload}); err != nil {
		return err
	}
	p, err := rn.ensureClaude(ctx, r, permissionMode)
	if err != nil {
		return err
	}
	if r.nativeMeta == nil {
		r.nativeMeta = map[string]any{"title": r.title}
	}
	r.startedAt = store.Now()
	if err := r.append(ctx,
		event{typ: "provider-thread.updated", driver: r.driver, payload: r.providerThread("active")},
		event{typ: "run.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.runPayload("running", nil, nil)},
		event{typ: "run-attempt.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.attemptPayload("running", nil)},
		event{typ: "node.updated", runID: r.ids.run, nodeID: r.ids.root, payload: r.rootNode("running", nil)},
	); err != nil {
		return err
	}
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
	nativeTurn := "turn:" + r.ids.attempt
	r.mu.Lock()
	r.turnID = nativeTurn
	r.providerTurn = "provider-turn:provider:" + r.driver + ":native-turn:" + encodeURIComponent(nativeTurn)
	r.mu.Unlock()
	r.turnRef = map[string]any{"driver": r.driver, "nativeId": nativeTurn, "strength": "weak"}
	r.turnStarted = store.Now()
	// The attachment budget counts the bytes of every message the session
	// was sent, as JSON strings.
	budget := map[string]any{"bytesUsed": 0, "pagesUsed": 0}
	if b, ok := r.nativeMeta["claudeAttachmentBudget"].(map[string]any); ok {
		budget = map[string]any{"bytesUsed": b["bytesUsed"], "pagesUsed": b["pagesUsed"]}
	}
	quoted, _ := json.Marshal(r.text)
	budget["bytesUsed"] = jsonNumber(budget["bytesUsed"]) + len(quoted)
	meta := map[string]any{}
	for k, v := range r.nativeMeta {
		meta[k] = v
	}
	meta["claudeAttachmentBudget"] = budget
	r.nativeMeta = meta
	if err := r.append(ctx,
		event{cause: sessionCause, typ: "provider-turn.updated", runID: r.ids.run, nodeID: r.ids.root, driver: r.driver,
			payload: r.providerTurnPayload("running", nil)},
		event{cause: sessionCause, typ: "provider-thread.updated", runID: r.ids.run, nodeID: r.ids.root, driver: r.driver,
			payload: r.providerThread("active")},
	); err != nil {
		return err
	}
	u, err := newUUID()
	if err != nil {
		return err
	}
	text := rn.srv.textWithAttachmentPaths(r.text, r.attachments)
	images, err := rn.srv.agentImages(r.attachments)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		err = p.SendUser(text, u)
	} else {
		// Upstream sends the images first, then the text.
		var content []any
		for _, img := range images {
			content = append(content, map[string]any{"type": "image",
				"source": map[string]any{"type": "base64", "media_type": img.mime, "data": img.data}})
		}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		err = p.SendUserContent(content, u)
	}
	if err != nil {
		return err
	}
	return rn.followClaude(ctx, p, r)
}

func jsonNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// claudeTool is a tool call the turn shows as an item.
type claudeTool struct {
	id, name, itemType, kind string
	input                    map[string]any
	startedAt                string
}

// claudeToolTypes are upstream's item types and request kinds for Claude
// Code's tools (ClaudeAdapterV2 CLAUDE_KNOWN_TOOL_CLASSIFICATIONS).
var claudeToolTypes = map[string][2]string{
	"agent": {"dynamic_tool", "command"}, "bash": {"command_execution", "command"},
	"edit": {"file_change", "file-change"}, "glob": {"dynamic_tool", "file-read"},
	"grep": {"dynamic_tool", "file-read"}, "ls": {"dynamic_tool", "file-read"},
	"monitor": {"dynamic_tool", "command"}, "multiedit": {"file_change", "file-change"},
	"notebookedit": {"file_change", "file-change"}, "read": {"dynamic_tool", "file-read"},
	"sendmessage": {"dynamic_tool", "command"}, "task": {"dynamic_tool", "command"},
	"taskstop": {"dynamic_tool", "command"}, "todowrite": {"dynamic_tool", "command"},
	"toolsearch": {"dynamic_tool", "command"}, "webfetch": {"web_search", "command"},
	"websearch": {"web_search", "command"}, "write": {"file_change", "file-change"},
}

func classifyClaudeTool(name string) (itemType, kind string) {
	if c, ok := claudeToolTypes[strings.ToLower(name)]; ok {
		return c[0], c[1]
	}
	return "dynamic_tool", "command"
}

// claudeTurn is what followClaude knows about the turn it handles.
type claudeTurn struct {
	*turn
	p       *claude.Process
	msgID   string         // the API message being streamed
	blocks  map[string]int // API message id -> blocks completed
	streams map[int]string // content block index -> text key, while streaming
	tools   map[string]*claudeTool
	usage   struct{ input, cacheRead, cacheCreation, output int }
}

func (ct *claudeTurn) usageUpdate(ctx context.Context) error {
	r := ct.r
	window := claudeContextWindow(r.model)
	u := ct.usage
	r.turnRef = map[string]any{"driver": r.driver, "nativeId": r.turnID, "strength": "strong"}
	r.tokenUsage = map[string]any{
		"usedTokens": u.input + u.cacheRead + u.cacheCreation + u.output, "maxTokens": window,
		"inputTokens": u.input + u.cacheCreation, "cachedInputTokens": u.cacheRead, "outputTokens": u.output,
		"reasoningOutputTokens": 0, "updatedAt": store.Now(),
	}
	if threshold := claudeAutoCompactThreshold(r.cwd, r.model, window); threshold > 0 {
		r.tokenUsage["autoCompactThreshold"] = threshold
	}
	return r.append(ctx, event{cause: ct.sessionCause, typ: "provider-turn.updated", runID: r.ids.run, nodeID: r.ids.root,
		driver: r.driver, payload: r.providerTurnPayload("running", nil)})
}

type claudeUsage struct {
	InputTokens   *int `json:"input_tokens"`
	OutputTokens  *int `json:"output_tokens"`
	CacheRead     *int `json:"cache_read_input_tokens"`
	CacheCreation *int `json:"cache_creation_input_tokens"`
}

func (ct *claudeTurn) addUsage(u *claudeUsage) {
	if u == nil {
		return
	}
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	set(&ct.usage.input, u.InputTokens)
	set(&ct.usage.output, u.OutputTokens)
	set(&ct.usage.cacheRead, u.CacheRead)
	set(&ct.usage.cacheCreation, u.CacheCreation)
}

// followClaude handles the CLI's messages for the turn until its result.
func (rn *runner) followClaude(ctx context.Context, p *claude.Process, r *runState) error {
	ct := &claudeTurn{
		turn: &turn{
			rn: rn, r: r, sessionCause: "provider-session:" + encodeURIComponent(r.ids.session),
			started: map[string]string{}, ordinals: map[string]int{}, texts: map[string]*textItem{},
			fileKinds: map[string]map[string]any{}, responses: make(chan requestAnswer, 4),
		},
		p: p, blocks: map[string]int{}, streams: map[int]string{}, tools: map[string]*claudeTool{},
	}
	r.mu.Lock()
	r.answers = ct.responses
	r.mu.Unlock()
	const flushEvery = 150 * time.Millisecond
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	stop := r.stop
	stopped := false
	for {
		select {
		case <-stop:
			stop, stopped = nil, true
			go func() {
				ictx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, _ = p.Control(ictx, map[string]any{"subtype": "interrupt"})
			}()
		case <-ticker.C:
			if err := ct.flush(ctx); err != nil {
				return err
			}
		case a := <-ct.responses:
			if err := ct.claudeAnswer(a); err != nil {
				return err
			}
		case m, ok := <-p.Messages():
			if !ok {
				if stopped {
					return rn.finish(ctx, r, "interrupted")
				}
				return fmt.Errorf("claude exited during the turn: %v", p.Err())
			}
			done, err := ct.message(ctx, m, stopped)
			if err != nil || done {
				return err
			}
		}
	}
}

// message handles one CLI message; done reports the turn's end.
func (ct *claudeTurn) message(ctx context.Context, m claude.Message, stopped bool) (bool, error) {
	r := ct.r
	if m.Parent != nil && *m.Parent != "" {
		return false, nil // a subagent's messages
	}
	switch m.Type {
	case "stream_event":
		return false, ct.streamEvent(ctx, m.Event)
	case "assistant":
		var msg struct {
			ID      string            `json:"id"`
			Model   string            `json:"model"`
			Content []json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(m.Message, &msg)
		if r.turnModel == "" && msg.Model != "" && msg.Model != "<synthetic>" {
			r.turnModel = msg.Model
		}
		for _, raw := range msg.Content {
			index := ct.blocks[msg.ID]
			ct.blocks[msg.ID]++
			if err := ct.block(ctx, msg.ID, index, raw); err != nil {
				return false, err
			}
		}
	case "user":
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(m.Message, &msg)
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return false, nil
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				if err := ct.toolResult(ctx, b.ToolUseID, toolResultText(b.Content), b.IsError); err != nil {
					return false, err
				}
			}
		}
	case "control_request":
		return false, ct.controlRequest(ctx, m)
	case "result":
		if err := ct.flush(ctx); err != nil {
			return true, err
		}
		var res struct {
			Subtype string       `json:"subtype"`
			IsError bool         `json:"is_error"`
			UUID    string       `json:"uuid"`
			Usage   *claudeUsage `json:"usage"`
		}
		_ = json.Unmarshal(m.Raw, &res)
		status := "completed"
		switch {
		case stopped:
			status = "interrupted"
		case res.IsError || (res.Subtype != "" && res.Subtype != "success"):
			status = "failed"
		}
		if status != "interrupted" {
			if res.UUID != "" {
				r.turnRef = map[string]any{"driver": r.driver, "nativeId": res.UUID, "strength": "weak"}
			}
			if res.Usage != nil {
				ct.addUsage(res.Usage)
			}
			u := ct.usage
			models := []any{}
			if r.turnModel != "" {
				models = append(models, r.turnModel)
			}
			r.turnUsage = map[string]any{
				"usageScope": "main_agent", "cachedInputTokens": u.cacheRead, "cacheCreationTokens": u.cacheCreation,
				"hasSubagents": false, "models": models, "usageStatus": "complete",
				"inputTokens": u.input, "outputTokens": u.output,
			}
		} else {
			r.turnRef = map[string]any{"driver": r.driver, "nativeId": r.turnID, "strength": "weak"}
		}
		return true, ct.rn.finish(ctx, r, status)
	}
	return false, nil
}

func (ct *claudeTurn) streamEvent(ctx context.Context, raw json.RawMessage) error {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string       `json:"id"`
			Usage *claudeUsage `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		Usage *claudeUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return nil
	}
	switch e.Type {
	case "message_start":
		ct.msgID = e.Message.ID
		ct.streams = map[int]string{}
		ct.addUsage(e.Message.Usage)
		return ct.usageUpdate(ctx)
	case "message_delta":
		ct.addUsage(e.Usage)
		return ct.usageUpdate(ctx)
	case "content_block_start":
		switch e.ContentBlock.Type {
		case "text", "thinking":
			key := fmt.Sprintf("%s:%s:%d", ct.msgID, e.ContentBlock.Type, e.Index)
			ct.streams[e.Index] = key
			ct.startedAt(key, "")
			ct.ordinal(itemTurnItemID(ct.r.driver, key))
			ct.text(key, key, e.ContentBlock.Type == "thinking")
		}
	case "content_block_delta":
		key, ok := ct.streams[e.Index]
		if !ok {
			return nil
		}
		x := ct.texts[key]
		if x == nil {
			return nil
		}
		switch e.Delta.Type {
		case "text_delta":
			x.text.WriteString(e.Delta.Text)
		case "thinking_delta":
			x.text.WriteString(e.Delta.Thinking)
		}
	}
	return nil
}

// block handles one finished content block of an assistant message.
func (ct *claudeTurn) block(ctx context.Context, msgID string, index int, raw json.RawMessage) error {
	var b struct {
		Type     string         `json:"type"`
		Text     string         `json:"text"`
		Thinking string         `json:"thinking"`
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Input    map[string]any `json:"input"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	switch b.Type {
	case "text", "thinking":
		key := fmt.Sprintf("%s:%s:%d", msgID, b.Type, index)
		ct.startedAt(key, "")
		ct.ordinal(itemTurnItemID(ct.r.driver, key))
		x := ct.text(key, key, b.Type == "thinking")
		final := b.Text
		if b.Type == "thinking" {
			final = b.Thinking
		}
		if strings.TrimSpace(final) == "" && x.text.Len() == 0 {
			delete(ct.texts, key)
			return nil
		}
		return ct.complete(ctx, x, final)
	case "tool_use":
		itemType, kind := classifyClaudeTool(b.Name)
		if b.Input == nil {
			b.Input = map[string]any{}
		}
		tool := &claudeTool{id: b.ID, name: b.Name, itemType: itemType, kind: kind, input: b.Input,
			startedAt: ct.startedAt(b.ID, "")}
		ct.tools[b.ID] = tool
		return ct.r.append(ctx, ct.toolEvents(tool, "running", nil, "")...)
	}
	return nil
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// toolEvents are the node and turn item of a tool call in its state.
func (ct *claudeTurn) toolEvents(tool *claudeTool, status string, completedAt any, output string) []event {
	nodeID, itemID := itemNodeID(ct.r.driver, tool.id), itemTurnItemID(ct.r.driver, tool.id)
	extra := map[string]any{}
	switch tool.itemType {
	case "command_execution":
		extra["input"] = stringField(tool.input, "command", "cmd")
		if output != "" {
			extra["output"] = output
		}
	case "file_change":
		extra["fileName"] = stringField(tool.input, "file_path", "notebook_path", "path")
		if output != "" {
			extra["diffStr"] = output
		}
	case "web_search":
		if q := stringField(tool.input, "query", "url"); q != "" {
			extra["patterns"] = []any{q}
		}
	default:
		extra["toolName"] = tool.name
		extra["input"] = tool.input
		if output != "" {
			extra["output"] = output
		}
	}
	return []event{
		ct.item(nodeID, "node.updated", ct.node(nodeID, "tool_call", status, tool.startedAt, completedAt, tool.id, nil, "")),
		ct.item(nodeID, "turn-item.updated", ct.turnItem(itemID, nodeID, tool.itemType, status, tool.startedAt, completedAt, tool.id, extra)),
	}
}

func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Type == "text" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func (ct *claudeTurn) toolResult(ctx context.Context, toolUseID, output string, isError bool) error {
	tool := ct.tools[toolUseID]
	if tool == nil {
		return nil
	}
	delete(ct.tools, toolUseID)
	status := "completed"
	if isError {
		status = "failed"
	}
	return ct.r.append(ctx, ct.toolEvents(tool, status, store.Now(), output)...)
}

// controlRequest handles a request from the CLI: can_use_tool asks the
// user to approve a tool call or, for AskUserQuestion, to answer.
func (ct *claudeTurn) controlRequest(ctx context.Context, m claude.Message) error {
	var req struct {
		Subtype   string         `json:"subtype"`
		ToolName  string         `json:"tool_name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	_ = json.Unmarshal(m.Request, &req)
	if req.Subtype != "can_use_tool" {
		return ct.p.Send(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": m.RequestID, "error": "yatt-server does not handle " + req.Subtype,
		}})
	}
	if req.Input == nil {
		req.Input = map[string]any{}
	}
	r := ct.r
	if req.ToolName == "AskUserQuestion" {
		return ct.claudeQuestion(ctx, m.RequestID, req.ToolUseID, req.Input)
	}
	_, kind := classifyClaudeTool(req.ToolName)
	if ct.rn.allowed(r.nativeThread, kind) {
		return ct.p.Respond(m.RequestID, map[string]any{"behavior": "allow", "updatedInput": req.Input})
	}
	var prompt any
	switch kind {
	case "command":
		if c := stringField(req.Input, "command", "description"); c != "" {
			prompt = c
		}
	default:
		if f := stringField(req.Input, "file_path", "notebook_path", "path", "pattern"); f != "" {
			prompt = f
		}
	}
	id, err := ct.requestID(req.ToolUseID)
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
	ct.ordinal(itemID)
	item := ct.turnItem(itemID, nodeID, "approval_request", "waiting", now, nil, req.ToolUseID, extra)
	item["updatedAt"] = now
	request := ct.runtimeRequest(id, nodeID, req.ToolUseID, kind, now)
	if kind == "command" {
		request["responseCapability"] = map[string]any{"type": "live", "providerSessionId": r.ids.session, "supportsFeedback": true}
	}
	if err := r.append(ctx,
		ct.item(nodeID, "node.updated", ct.node(nodeID, "approval_request", "waiting", now, nil, req.ToolUseID, id, itemNodeID(r.driver, req.ToolUseID))),
		ct.item(nodeID, "runtime-request.updated", request),
		ct.item(nodeID, "turn-item.updated", item),
	); err != nil {
		return err
	}
	ct.rn.waitFor(id, &pendingRequest{run: r, claudeID: m.RequestID, claudeInput: req.Input, kind: kind, answerCh: ct.responses})
	return nil
}

func (ct *claudeTurn) claudeQuestion(ctx context.Context, requestID, toolUseID string, input map[string]any) error {
	r := ct.r
	id, err := ct.requestID(toolUseID)
	if err != nil {
		return err
	}
	now := store.Now()
	questions := []any{}
	native := map[string]string{}
	qs, _ := input["questions"].([]any)
	for i, q := range qs {
		qm, _ := q.(map[string]any)
		text := stringField(qm, "question")
		qid := text
		if qid == "" {
			qid = fmt.Sprintf("question-%d", i+1)
		}
		native[qid] = qid
		options := []any{}
		if os, ok := qm["options"].([]any); ok {
			for j, o := range os {
				om, _ := o.(map[string]any)
				label := stringField(om, "label")
				if label == "" {
					label = fmt.Sprintf("Option %d", j+1)
				}
				desc := stringField(om, "description")
				if desc == "" {
					desc = label
				}
				options = append(options, map[string]any{"label": label, "description": desc})
			}
		}
		header := stringField(qm, "header")
		if header == "" {
			header = "Question"
		}
		if text == "" {
			text = "Choose an answer."
		}
		multi, _ := qm["multiSelect"].(bool)
		questions = append(questions, map[string]any{
			"id": qid, "header": header, "question": text, "options": options, "multiSelect": multi,
		})
	}
	nodeID := "node:runtime-request:" + encodeURIComponent(id)
	itemID := "turn-item:runtime-request:" + encodeURIComponent(id)
	ct.ordinal(itemID)
	item := ct.turnItem(itemID, nodeID, "user_input_request", "waiting", now, nil, toolUseID,
		map[string]any{"requestId": id, "questions": questions})
	item["updatedAt"] = now
	if err := r.append(ctx,
		ct.item(nodeID, "node.updated", ct.node(nodeID, "user_input_request", "waiting", now, nil, toolUseID, id, itemNodeID(r.driver, toolUseID))),
		ct.item(nodeID, "runtime-request.updated", ct.runtimeRequest(id, nodeID, toolUseID, "user_input", now)),
		ct.item(nodeID, "turn-item.updated", item),
	); err != nil {
		return err
	}
	ct.rn.waitFor(id, &pendingRequest{run: r, claudeID: requestID, claudeInput: input, kind: "user_input", native: native, answerCh: ct.responses})
	return nil
}

// claudeAnswer sends the user's response to the CLI.
func (ct *claudeTurn) claudeAnswer(a requestAnswer) error {
	pr := ct.rn.takePending(a.requestID)
	if pr == nil {
		return nil
	}
	if pr.kind == "user_input" {
		answers := map[string]any{}
		for qid, v := range a.answers {
			if _, ok := pr.native[qid]; ok {
				answers[qid] = v
			}
		}
		updated := map[string]any{}
		for k, v := range pr.claudeInput {
			updated[k] = v
		}
		updated["answers"] = answers
		return ct.p.Respond(pr.claudeID, map[string]any{"behavior": "allow", "updatedInput": updated})
	}
	switch a.decision {
	case "accept", "acceptForSession", "acceptAlways":
		if a.decision != "accept" {
			ct.rn.allow(ct.r.nativeThread, pr.kind)
		}
		return ct.p.Respond(pr.claudeID, map[string]any{"behavior": "allow", "updatedInput": pr.claudeInput})
	}
	return ct.p.Respond(pr.claudeID, map[string]any{"behavior": "deny", "message": "User declined tool execution."})
}
