package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Thread terminals (spec/behavior/terminals.md), after upstream's
// TerminalManager: each (thread, terminal id) pair owns one shell in a PTY.
// Its output is kept as bounded history, so a client that attaches later
// sees what was printed, and every change is an event with a sequence
// number that attach and observe streams forward.

const (
	terminalHistoryLines = 5000
	terminalHistoryBytes = 8 << 20
	terminalDefaultCols  = 120
	terminalDefaultRows  = 30
	terminalLabelMax     = 128
	terminalKillGrace    = time.Second
)

var terminalEnvBlocklist = map[string]bool{
	"PORT": true, "ELECTRON_RENDERER_PORT": true, "ELECTRON_RUN_AS_NODE": true,
	"TMUX": true, "TMUX_PANE": true, "STY": true, "WINDOW": true,
	"ZELLIJ": true, "ZELLIJ_SESSION_NAME": true, "ZELLIJ_PANE_ID": true,
}

type terminalSession struct {
	threadID, terminalID string
	cwd                  string
	worktreePath         *string
	status               string // starting, running, exited, error
	pid                  *int
	history              terminalHistory
	pendingControl       string
	exitCode, exitSignal *int
	updatedAt            string
	sequence             int
	cols, rows           int
	runtimeEnv           map[string]string
	startupError         string

	ptmx *os.File
	cmd  *exec.Cmd
}

type terminalListener struct {
	mu      sync.Mutex
	queue   []map[string]any
	notify  chan struct{}
	filter  func(map[string]any) bool
	stopped bool
}

func (l *terminalListener) push(e map[string]any) {
	if l.filter != nil && !l.filter(e) {
		return
	}
	l.mu.Lock()
	l.queue = append(l.queue, e)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *terminalListener) drain() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.queue
	l.queue = nil
	return q
}

type terminalManager struct {
	mu        sync.Mutex
	sessions  map[string]*terminalSession
	closed    map[string]bool
	listeners map[*terminalListener]bool
}

func newTerminalManager() *terminalManager {
	return &terminalManager{
		sessions:  map[string]*terminalSession{},
		closed:    map[string]bool{},
		listeners: map[*terminalListener]bool{},
	}
}

func terminalKey(threadID, terminalID string) string { return threadID + "\x00" + terminalID }

var terminalNumberRe = regexp.MustCompile(`(?i)^term(?:inal)?-(\d+)(?:-[\da-f]{8}(?:-[\da-f]{4}){3}-[\da-f]{12})?$`)

// terminalLabel is upstream's getTerminalLabel: "term-2" is "Terminal 2".
func terminalLabel(terminalID string) string {
	label := terminalID
	if m := terminalNumberRe.FindStringSubmatch(terminalID); m != nil {
		label = "Terminal " + m[1]
	}
	if len(label) > terminalLabelMax {
		label = label[:terminalLabelMax]
	}
	return label
}

func (t *terminalSession) stamp() {
	t.sequence++
	t.updatedAt = store.Now()
}

func (t *terminalSession) snapshot() map[string]any {
	out := map[string]any{
		"threadId": t.threadID, "terminalId": t.terminalID, "cwd": t.cwd,
		"worktreePath": t.worktreePath, "status": t.status, "pid": t.pid,
		"history": t.history.value(), "exitCode": t.exitCode, "exitSignal": t.exitSignal,
		"label": terminalLabel(t.terminalID), "updatedAt": t.updatedAt,
		"sequence": t.sequence, "cols": t.cols, "rows": t.rows,
	}
	if t.startupError != "" {
		out["error"] = t.startupError
	}
	return out
}

func (t *terminalSession) summary() map[string]any {
	return map[string]any{
		"threadId": t.threadID, "terminalId": t.terminalID, "cwd": t.cwd,
		"worktreePath": t.worktreePath, "status": t.status, "pid": t.pid,
		"exitCode": t.exitCode, "exitSignal": t.exitSignal,
		"hasRunningSubprocess": false, "label": terminalLabel(t.terminalID),
		"updatedAt": t.updatedAt,
	}
}

// publish sends an event to every listener; callers hold m.mu.
func (m *terminalManager) publish(e map[string]any) {
	for l := range m.listeners {
		l.push(e)
	}
}

func (m *terminalManager) listen(filter func(map[string]any) bool) *terminalListener {
	l := &terminalListener{notify: make(chan struct{}, 1), filter: filter}
	m.listeners[l] = true
	return l
}

func (m *terminalManager) unlisten(l *terminalListener) {
	m.mu.Lock()
	delete(m.listeners, l)
	m.mu.Unlock()
}

// --- errors ------------------------------------------------------------------

func terminalLookupError(threadID, terminalID string) error {
	return &TypedError{Tag: "TerminalSessionLookupError", Fields: map[string]any{
		"threadId": threadID, "terminalId": terminalID,
	}, Text: fmt.Sprintf("Unknown terminal thread: %s, terminal: %s", threadID, terminalID)}
}

func terminalNotRunningError(threadID, terminalID string) error {
	return &TypedError{Tag: "TerminalNotRunningError", Fields: map[string]any{
		"threadId": threadID, "terminalId": terminalID,
	}, Text: fmt.Sprintf("Terminal is not running for thread: %s, terminal: %s", threadID, terminalID)}
}

func checkTerminalCwd(cwd string) error {
	info, err := os.Stat(cwd)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &TypedError{Tag: "TerminalCwdNotFoundError", Fields: map[string]any{"cwd": cwd}, Text: fmt.Sprintf("Terminal folder does not exist: %s. Restore the folder at this path before retrying.", cwd)}
	case err != nil:
		return &TypedError{Tag: "TerminalCwdStatError", Fields: map[string]any{"cwd": cwd}, Text: fmt.Sprintf("Failed to access terminal cwd: %s", cwd)}
	case !info.IsDir():
		return &TypedError{Tag: "TerminalCwdNotDirectoryError", Fields: map[string]any{"cwd": cwd}, Text: fmt.Sprintf("Terminal cwd is not a directory: %s", cwd)}
	}
	return nil
}

// --- process lifecycle -------------------------------------------------------

func terminalSpawnEnv(runtimeEnv map[string]string, threadID string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "T3CODE_") || strings.HasPrefix(upper, "VITE_") || terminalEnvBlocklist[upper] {
			continue
		}
		env[k] = v
	}
	env["TERM_PROGRAM"] = "t3code"
	env["TERM_PROGRAM_VERSION"] = Version
	home, _ := os.UserHomeDir()
	for k, v := range runtimeEnv {
		if home != "" && (v == "~" || strings.HasPrefix(v, "~/")) {
			v = home + v[1:]
		}
		env[k] = v
	}
	env["T3CODE_THREAD_ID"] = threadID
	if _, ok := env["COLORTERM"]; !ok {
		env["COLORTERM"] = "truecolor"
	}
	if env["TERM"] == "" {
		env["TERM"] = "xterm-256color"
	}
	if f := env["fish_features"]; !strings.Contains(f, "query-term") {
		if f == "" {
			env["fish_features"] = "no-query-term"
		} else {
			env["fish_features"] = f + ",no-query-term"
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// terminalShell is upstream's shell choice on POSIX: the first of
// T3CODE_TERMINAL_SHELL, $SHELL, zsh, bash and sh that resolves, with the
// arguments upstream gives zsh and bash. It returns the resolved path, the
// arguments and the command as tried, for the error message.
func terminalShell(runtimeEnv map[string]string) (string, []string, string) {
	configured := runtimeEnv["T3CODE_TERMINAL_SHELL"]
	if configured == "" {
		configured = os.Getenv("T3CODE_TERMINAL_SHELL")
	}
	candidates := []string{strings.TrimSpace(configured), os.Getenv("SHELL"),
		"/bin/zsh", "/bin/bash", "/bin/sh", "zsh", "bash", "sh"}
	var tried []string
	for _, c := range candidates {
		if c == "" {
			continue
		}
		var args []string
		switch filepath.Base(c) {
		case "zsh":
			args = []string{"-o", "nopromptsp"}
		case "bash":
			if runtime.GOOS == "darwin" {
				args = []string{"-l"}
			}
		}
		tried = append(tried, strings.Join(append([]string{c}, args...), " "))
		if path, err := exec.LookPath(c); err == nil {
			return path, args, ""
		}
	}
	return "", nil, strings.Join(tried, ", ")
}

// start runs the session's shell; callers hold m.mu. eventType is
// "started" or "restarted".
func (m *terminalManager) start(t *terminalSession, eventType string) {
	m.stop(t)
	t.status = "starting"
	t.startupError = ""
	t.exitCode, t.exitSignal = nil, nil
	t.updatedAt = store.Now()
	path, args, tried := terminalShell(t.runtimeEnv)
	var err error = fmt.Errorf("no shell found (attempted shells: %s)", tried)
	if path != "" {
		cmd := exec.Command(path, args...)
		cmd.Dir = t.cwd
		cmd.Env = terminalSpawnEnv(t.runtimeEnv, t.threadID)
		var ptmx *os.File
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(t.cols), Rows: uint16(t.rows)})
		if err == nil {
			pid := cmd.Process.Pid
			t.cmd, t.ptmx, t.pid = cmd, ptmx, &pid
			t.status = "running"
			t.stamp()
			m.publish(map[string]any{"type": eventType, "threadId": t.threadID, "terminalId": t.terminalID,
				"sequence": t.sequence, "snapshot": t.snapshot()})
			go m.pump(t, cmd, ptmx)
			return
		}
	}
	t.status = "error"
	t.startupError = fmt.Sprintf("Failed to spawn PTY process: %v", err)
	t.pid = nil
	t.stamp()
	m.publish(map[string]any{"type": "error", "threadId": t.threadID, "terminalId": t.terminalID,
		"sequence": t.sequence, "message": t.startupError})
}

// pump forwards a shell's output until it exits.
func (m *terminalManager) pump(t *terminalSession, cmd *exec.Cmd, ptmx *os.File) {
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			cut := len(data)
			// Hold back a rune the read split, so output events are UTF-8.
			for i := 1; i < utf8.UTFMax && i <= len(data); i++ {
				if utf8.RuneStart(data[len(data)-i]) {
					if !utf8.FullRune(data[len(data)-i:]) {
						cut = len(data) - i
					}
					break
				}
			}
			carry = append([]byte(nil), data[cut:]...)
			m.output(t, cmd, string(data[:cut]))
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.cmd != cmd {
		return
	}
	_ = ptmx.Close()
	t.cmd, t.ptmx, t.pid = nil, nil, nil
	t.status = "exited"
	t.pendingControl = ""
	// As node-pty reports it: the exit status and the signal, 0 for none.
	code, sig := 0, 0
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			sig = int(ws.Signal())
		} else {
			code = ws.ExitStatus()
		}
	}
	t.exitCode, t.exitSignal = &code, &sig
	t.stamp()
	m.publish(map[string]any{"type": "exited", "threadId": t.threadID, "terminalId": t.terminalID,
		"sequence": t.sequence, "exitCode": t.exitCode, "exitSignal": t.exitSignal})
}

func (m *terminalManager) output(t *terminalSession, cmd *exec.Cmd, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.cmd != cmd || t.status != "running" {
		return
	}
	visible, pending := sanitizeTerminalHistory(t.pendingControl, data)
	t.pendingControl = pending
	t.history.append(visible)
	t.stamp()
	m.publish(map[string]any{"type": "output", "threadId": t.threadID, "terminalId": t.terminalID,
		"sequence": t.sequence, "data": data})
}

// stop ends the session's shell: SIGTERM to its process group, then
// SIGKILL after a grace period. Callers hold m.mu.
func (m *terminalManager) stop(t *terminalSession) {
	cmd, ptmx := t.cmd, t.ptmx
	if cmd == nil {
		return
	}
	t.cmd, t.ptmx, t.pid = nil, nil, nil
	t.status = "exited"
	t.pendingControl = ""
	t.updatedAt = store.Now()
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = cmd.Process.Signal(syscall.SIGHUP)
	go func() {
		time.Sleep(terminalKillGrace)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = ptmx.Close()
	}()
}

func (m *terminalManager) closeSession(threadID, terminalID string) {
	key := terminalKey(threadID, terminalID)
	m.closed[key] = true
	t := m.sessions[key]
	if t == nil {
		return
	}
	m.stop(t)
	delete(m.sessions, key)
	m.publish(map[string]any{"type": "closed", "threadId": threadID, "terminalId": terminalID, "sequence": t.sequence + 1})
}

func (m *terminalManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.sessions {
		m.stop(t)
	}
}

// --- requests ----------------------------------------------------------------

type terminalInput struct {
	ThreadID            string             `json:"threadId"`
	TerminalID          *string            `json:"terminalId"`
	Cwd                 *string            `json:"cwd"`
	WorktreePath        json.RawMessage    `json:"worktreePath"`
	Cols                *int               `json:"cols"`
	Rows                *int               `json:"rows"`
	Env                 map[string]*string `json:"env"`
	Resume              *bool              `json:"resume"`
	RestartIfNotRunning *bool              `json:"restartIfNotRunning"`
	MaxHistoryBytes     *int               `json:"maxHistoryBytes"`
	Data                string             `json:"data"`
	DeleteHistory       *bool              `json:"deleteHistory"`
}

func (in terminalInput) terminalID() string {
	if in.TerminalID == nil {
		return ""
	}
	return *in.TerminalID
}

func (in terminalInput) runtimeEnv() map[string]string {
	if len(in.Env) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in.Env {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

// worktree is the request's worktreePath: absent (nil, false), null
// (nil, true) or a path.
func (in terminalInput) worktree() (*string, bool) {
	if len(in.WorktreePath) == 0 {
		return nil, false
	}
	var p *string
	_ = json.Unmarshal(in.WorktreePath, &p)
	return p, true
}

// prepareTerminalInput is upstream's prepareTerminalLaunch: a terminal of a
// worktree thread whose cwd is inside the worktree records that worktree.
func (s *Server) prepareTerminalInput(in *terminalInput) {
	var worktree *string
	s.store.Read(func(st *store.State, _ int64) {
		if t := st.Threads[in.ThreadID]; t != nil {
			worktree = t.WorktreePath
		}
	})
	if worktree == nil {
		return
	}
	cwd := *worktree
	if in.Cwd != nil {
		cwd = *in.Cwd
	}
	rel, err := filepath.Rel(*worktree, filepath.Clean(cwd))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	in.WorktreePath, _ = json.Marshal(*worktree)
}

// open is upstream's openLocked; callers hold m.mu.
func (m *terminalManager) open(in terminalInput) (map[string]any, error) {
	if err := checkTerminalCwd(*in.Cwd); err != nil {
		return nil, err
	}
	id := in.terminalID()
	key := terminalKey(in.ThreadID, id)
	worktree, hasWorktree := in.worktree()
	env := in.runtimeEnv()
	t := m.sessions[key]
	if t == nil {
		t = &terminalSession{
			threadID: in.ThreadID, terminalID: id, cwd: *in.Cwd, worktreePath: worktree,
			status: "starting", cols: terminalDefaultCols, rows: terminalDefaultRows,
			runtimeEnv: env, updatedAt: store.Now(),
		}
		if in.Cols != nil {
			t.cols = *in.Cols
		}
		if in.Rows != nil {
			t.rows = *in.Rows
		}
		m.sessions[key] = t
		delete(m.closed, key)
		m.start(t, "started")
		return t.snapshot(), nil
	}
	nextWorktree := t.worktreePath
	if hasWorktree {
		nextWorktree = worktree
	}
	if t.cwd != *in.Cwd || !sameEnv(t.runtimeEnv, env) || !sameStringPtr(t.worktreePath, nextWorktree) {
		m.stop(t)
		t.cwd, t.worktreePath, t.runtimeEnv = *in.Cwd, nextWorktree, env
		t.history.clear()
		t.pendingControl = ""
	} else if t.status == "exited" || t.status == "error" {
		t.runtimeEnv, t.worktreePath = env, nextWorktree
		t.history.clear()
		t.pendingControl = ""
	}
	if in.Cols != nil {
		t.cols = *in.Cols
	}
	if in.Rows != nil {
		t.rows = *in.Rows
	}
	if t.cmd == nil {
		m.start(t, "started")
		return t.snapshot(), nil
	}
	m.resize(t, t.cols, t.rows)
	return t.snapshot(), nil
}

// resize is upstream's resizeSession: the PTY takes the new size and
// viewers get an activity event. Callers hold m.mu.
func (m *terminalManager) resize(t *terminalSession, cols, rows int) {
	if t.ptmx != nil {
		_ = pty.Setsize(t.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	t.cols, t.rows = cols, rows
	t.stamp()
	m.publish(map[string]any{"type": "activity", "threadId": t.threadID, "terminalId": t.terminalID,
		"sequence": t.sequence, "hasRunningSubprocess": false, "label": terminalLabel(t.terminalID),
		"cols": cols, "rows": rows})
}

func sameEnv(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func sameStringPtr(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (s *Server) terminalOpen(ctx context.Context, payload json.RawMessage) (any, error) {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.Cwd == nil {
		return nil, errors.New("cwd is required")
	}
	s.prepareTerminalInput(&in)
	m := s.terminals
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open(in)
}

func (s *Server) terminalWrite(ctx context.Context, payload json.RawMessage) (any, error) {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	m := s.terminals
	m.mu.Lock()
	t := m.sessions[terminalKey(in.ThreadID, in.terminalID())]
	if t == nil {
		m.mu.Unlock()
		return nil, terminalLookupError(in.ThreadID, in.terminalID())
	}
	ptmx := t.ptmx
	status := t.status
	m.mu.Unlock()
	if ptmx == nil || status != "running" {
		if status == "exited" {
			return nil, nil
		}
		return nil, terminalNotRunningError(in.ThreadID, in.terminalID())
	}
	if _, err := ptmx.Write([]byte(in.Data)); err != nil {
		return nil, &TypedError{Tag: "TerminalWriteError", Fields: map[string]any{
			"threadId": in.ThreadID, "terminalId": in.terminalID()}, Text: "Failed to write to terminal"}
	}
	return nil, nil
}

func (s *Server) terminalResize(ctx context.Context, payload json.RawMessage) (any, error) {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.Cols == nil || in.Rows == nil {
		return nil, errors.New("cols and rows are required")
	}
	m := s.terminals
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.sessions[terminalKey(in.ThreadID, in.terminalID())]
	if t == nil || t.cmd == nil || t.status != "running" || (t.cols == *in.Cols && t.rows == *in.Rows) {
		return nil, nil
	}
	m.resize(t, *in.Cols, *in.Rows)
	return nil, nil
}

func (s *Server) terminalClear(ctx context.Context, payload json.RawMessage) (any, error) {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	m := s.terminals
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.sessions[terminalKey(in.ThreadID, in.terminalID())]
	if t == nil {
		return nil, terminalLookupError(in.ThreadID, in.terminalID())
	}
	t.history.clear()
	t.pendingControl = ""
	t.stamp()
	m.publish(map[string]any{"type": "cleared", "threadId": t.threadID, "terminalId": t.terminalID, "sequence": t.sequence})
	return nil, nil
}

func (s *Server) terminalClose(ctx context.Context, payload json.RawMessage) (any, error) {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	m := s.terminals
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.TerminalID != nil {
		m.closeSession(in.ThreadID, *in.TerminalID)
		return nil, nil
	}
	var ids []string
	for _, t := range m.sessions {
		if t.threadID == in.ThreadID {
			ids = append(ids, t.terminalID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		m.closeSession(in.ThreadID, id)
	}
	return nil, nil
}

// attachEvent is how a session event reaches an attach stream: "started"
// becomes a snapshot.
func attachEvent(e map[string]any) map[string]any {
	if e["type"] == "started" {
		return map[string]any{"type": "snapshot", "snapshot": e["snapshot"]}
	}
	return e
}

// streamTerminal sends the first value, then the session's later events.
// initial runs under m.mu, so no event falls between it and the stream.
func (s *Server) streamTerminal(ctx context.Context, threadID, terminalID string, maxHistory *int,
	initial func() (map[string]any, error), emit func(...any) error) error {
	m := s.terminals
	m.mu.Lock()
	first, err := initial()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	l := m.listen(func(e map[string]any) bool {
		return e["threadId"] == threadID && e["terminalId"] == terminalID
	})
	m.mu.Unlock()
	defer m.unlisten(l)
	trim := func(e map[string]any) map[string]any {
		if maxHistory == nil {
			return e
		}
		snap, ok := e["snapshot"].(map[string]any)
		if !ok {
			return e
		}
		copied := map[string]any{}
		for k, v := range snap {
			copied[k] = v
		}
		copied["history"] = retainedHistory(snap["history"].(string), *maxHistory)
		out := map[string]any{}
		for k, v := range e {
			out[k] = v
		}
		out["snapshot"] = copied
		return out
	}
	if err := emit(trim(first)); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.notify:
		}
		events := l.drain()
		values := make([]any, 0, len(events))
		for _, e := range events {
			values = append(values, trim(attachEvent(e)))
		}
		if len(values) > 0 {
			if err := emit(values...); err != nil {
				return err
			}
		}
	}
}

// retainedHistory keeps the last max bytes of history, starting at a line.
func retainedHistory(history string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(history) <= max {
		return history
	}
	tail := history[len(history)-max:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		return tail[i+1:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

func (s *Server) terminalAttach(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	s.prepareTerminalInput(&in)
	id := in.terminalID()
	return s.streamTerminal(ctx, in.ThreadID, id, in.MaxHistoryBytes, func() (map[string]any, error) {
		m := s.terminals
		key := terminalKey(in.ThreadID, id)
		t := m.sessions[key]
		closed := map[string]any{"type": "closed", "threadId": in.ThreadID, "terminalId": id}
		if t == nil {
			if in.Resume != nil && *in.Resume && m.closed[key] {
				return closed, nil
			}
			if in.Cwd == nil {
				return nil, terminalLookupError(in.ThreadID, id)
			}
			snap, err := m.open(in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"type": "snapshot", "snapshot": snap}, nil
		}
		resume := in.Resume != nil && *in.Resume
		if !resume && t.cmd == nil && in.Cwd != nil && in.RestartIfNotRunning != nil && *in.RestartIfNotRunning {
			snap, err := m.open(in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"type": "snapshot", "snapshot": snap}, nil
		}
		cols, rows := t.cols, t.rows
		if in.Cols != nil {
			cols = *in.Cols
		}
		if in.Rows != nil {
			rows = *in.Rows
		}
		if t.cmd != nil && t.status == "running" && (cols != t.cols || rows != t.rows) {
			m.resize(t, cols, rows)
		}
		return map[string]any{"type": "snapshot", "snapshot": t.snapshot()}, nil
	}, emit)
}

func (s *Server) terminalObserve(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	var in terminalInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	id := in.terminalID()
	return s.streamTerminal(ctx, in.ThreadID, id, nil, func() (map[string]any, error) {
		t := s.terminals.sessions[terminalKey(in.ThreadID, id)]
		if t == nil {
			return nil, terminalLookupError(in.ThreadID, id)
		}
		return map[string]any{"type": "snapshot", "snapshot": t.snapshot()}, nil
	}, emit)
}

func (s *Server) subscribeTerminalMetadata(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	m := s.terminals
	m.mu.Lock()
	terminals := make([]any, 0, len(m.sessions))
	list := make([]*terminalSession, 0, len(m.sessions))
	for _, t := range m.sessions {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.updatedAt != b.updatedAt {
			return a.updatedAt > b.updatedAt
		}
		if a.threadID != b.threadID {
			return a.threadID < b.threadID
		}
		return a.terminalID < b.terminalID
	})
	for _, t := range list {
		terminals = append(terminals, t.summary())
	}
	l := m.listen(func(e map[string]any) bool {
		switch e["type"] {
		case "output", "cleared":
			return false
		}
		return true
	})
	m.mu.Unlock()
	defer m.unlisten(l)
	if err := emit(map[string]any{"type": "snapshot", "terminals": terminals, "managerId": s.terminalManagerID}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.notify:
		}
		var values []any
		m.mu.Lock()
		for _, e := range l.drain() {
			threadID, _ := e["threadId"].(string)
			terminalID, _ := e["terminalId"].(string)
			if e["type"] == "closed" {
				values = append(values, map[string]any{"type": "remove", "threadId": threadID, "terminalId": terminalID})
				continue
			}
			if t := m.sessions[terminalKey(threadID, terminalID)]; t != nil {
				values = append(values, map[string]any{"type": "upsert", "terminal": t.summary()})
			}
		}
		m.mu.Unlock()
		if len(values) > 0 {
			if err := emit(values...); err != nil {
				return err
			}
		}
	}
}

// --- history -----------------------------------------------------------------

// terminalHistory keeps the newest terminalHistoryLines lines and
// terminalHistoryBytes bytes of output.
type terminalHistory struct {
	text  string
	lines int
}

func (h *terminalHistory) value() string { return h.text }
func (h *terminalHistory) clear()        { h.text, h.lines = "", 0 }

func (h *terminalHistory) append(s string) {
	if s == "" {
		return
	}
	h.text += s
	h.lines += strings.Count(s, "\n")
	if h.lines > terminalHistoryLines {
		i := 0
		for ; h.lines > terminalHistoryLines; h.lines-- {
			i += strings.IndexByte(h.text[i:], '\n') + 1
		}
		h.text = h.text[i:]
	}
	if len(h.text) > terminalHistoryBytes {
		cut := len(h.text) - terminalHistoryBytes
		for cut < len(h.text) && !utf8.RuneStart(h.text[cut]) {
			cut++
		}
		h.lines -= strings.Count(h.text[:cut], "\n")
		h.text = h.text[cut:]
	}
}

// sanitizeTerminalHistory drops, from what history keeps, the control
// sequences that make a terminal answer (status and cursor reports, device
// attributes, color queries), as upstream does, so that replaying history
// into a new view does not type answers into the shell. A sequence split
// across reads is held back until it completes.
func sanitizeTerminalHistory(pending, data string) (string, string) {
	input := pending + data
	if !strings.ContainsRune(input, 0x1b) {
		return input, ""
	}
	var out strings.Builder
	i := 0
	for i < len(input) {
		c := input[i]
		if c != 0x1b {
			j := strings.IndexByte(input[i:], 0x1b)
			if j < 0 {
				out.WriteString(input[i:])
				break
			}
			out.WriteString(input[i : i+j])
			i += j
			continue
		}
		if i+1 >= len(input) {
			return out.String(), input[i:]
		}
		switch input[i+1] {
		case '[':
			j := i + 2
			for j < len(input) && (input[j] < 0x40 || input[j] > 0x7e) {
				j++
			}
			if j >= len(input) {
				return out.String(), input[i:]
			}
			if !stripCSI(input[i+2:j], input[j]) {
				out.WriteString(input[i : j+1])
			}
			i = j + 1
		case ']', 'P':
			end := -1
			for j := i + 2; j < len(input); j++ {
				if input[j] == 0x07 && input[i+1] == ']' {
					end = j + 1
					break
				}
				if input[j] == 0x1b && j+1 < len(input) && input[j+1] == '\\' {
					end = j + 2
					break
				}
			}
			if end < 0 {
				return out.String(), input[i:]
			}
			content := strings.TrimSuffix(strings.TrimSuffix(input[i+2:end], "\x1b\\"), "\x07")
			strip := (input[i+1] == ']' && oscQueryRe.MatchString(content)) ||
				(input[i+1] == 'P' && dcsQueryRe.MatchString(content))
			if !strip {
				out.WriteString(input[i:end])
			}
			i = end
		default:
			if input[i+1] == 'Z' {
				i += 2
				continue
			}
			out.WriteString(input[i : i+2])
			i += 2
		}
	}
	return out.String(), ""
}

var (
	csiParamsRe       = regexp.MustCompile(`^[0-9;?]*$`)
	csiDARe           = regexp.MustCompile(`^[>=0-9;?]*$`)
	csiModeQueryRe    = regexp.MustCompile(`^[0-9;?]*\$$`)
	csiVersionQueryRe = regexp.MustCompile(`^>[0-9;]*$`)
	oscQueryRe        = regexp.MustCompile(`^(10|11|12);(?:\?|rgb:)`)
	dcsQueryRe        = regexp.MustCompile(`^[01]?[$+][qr]`)
)

func stripCSI(body string, final byte) bool {
	switch final {
	case 'n':
		return true
	case 'R':
		return csiParamsRe.MatchString(body)
	case 'c':
		return csiDARe.MatchString(body)
	case 'p', 'y':
		return csiModeQueryRe.MatchString(body)
	case 'q':
		return csiVersionQueryRe.MatchString(body)
	case 'u':
		return strings.HasPrefix(body, "?")
	}
	return false
}
