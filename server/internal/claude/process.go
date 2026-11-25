// Package claude runs the `claude` CLI (Claude Code) in its stream-json
// mode, the protocol the Claude Agent SDK speaks: one JSON object per line
// on the child's stdio. User messages and control requests go to stdin;
// system, stream_event, assistant, user and result messages, the CLI's own
// control requests (can_use_tool) and the answers to ours come back on
// stdout.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Message is one line from the CLI. Raw holds the whole object.
type Message struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	Parent    *string         `json:"parent_tool_use_id"`
	Raw       json.RawMessage `json:"-"`
}

// Process is one running CLI.
type Process struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	wmu      sync.Mutex
	mu       sync.Mutex
	next     atomic.Int64
	controls map[string]chan controlResult
	messages chan Message
	done     chan struct{}
	err      error
}

type controlResult struct {
	response json.RawMessage
	err      error
}

// Start runs bin with args in dir and reads its output until it exits.
// Messages other than answers to our control requests arrive on Messages.
func Start(bin string, args []string, dir string, env []string, stderr io.Writer) (*Process, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, stdin: stdin, controls: map[string]chan controlResult{},
		messages: make(chan Message, 256), done: make(chan struct{})}
	go p.read(stdout)
	return p, nil
}

func (p *Process) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m Message
		if err := json.Unmarshal(line, &m); err != nil || m.Type == "" {
			continue
		}
		m.Raw = line
		if m.Type == "control_response" {
			var r struct {
				Response struct {
					Subtype   string          `json:"subtype"`
					RequestID string          `json:"request_id"`
					Response  json.RawMessage `json:"response"`
					Error     string          `json:"error"`
				} `json:"response"`
			}
			_ = json.Unmarshal(line, &r)
			p.mu.Lock()
			ch := p.controls[r.Response.RequestID]
			delete(p.controls, r.Response.RequestID)
			p.mu.Unlock()
			if ch != nil {
				if r.Response.Subtype == "error" {
					ch <- controlResult{err: fmt.Errorf("claude: %s", r.Response.Error)}
				} else {
					ch <- controlResult{response: r.Response.Response}
				}
			}
			continue
		}
		p.messages <- m
	}
	err := p.cmd.Wait()
	if err == nil {
		err = errors.New("claude exited")
	}
	p.mu.Lock()
	p.err = fmt.Errorf("claude: %w", err)
	for id, ch := range p.controls {
		ch <- controlResult{err: p.err}
		delete(p.controls, id)
	}
	p.mu.Unlock()
	close(p.messages)
	close(p.done)
}

// Messages delivers the CLI's messages until it exits, then closes.
func (p *Process) Messages() <-chan Message { return p.messages }

// Done is closed when the CLI has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is why the CLI exited, once Done is closed.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Send writes one message.
func (p *Process) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Control sends a control request and waits for its answer.
func (p *Process) Control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	id := fmt.Sprintf("yatt_%d", p.next.Add(1))
	ch := make(chan controlResult, 1)
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	p.controls[id] = ch
	p.mu.Unlock()
	if err := p.Send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.response, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Respond answers a control request from the CLI.
func (p *Process) Respond(requestID string, response any) error {
	return p.Send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": requestID, "response": response,
	}})
}

// SendUser sends a user message with text content.
func (p *Process) SendUser(text, uuid string) error {
	return p.Send(map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil, "uuid": uuid,
	})
}

// SendUserContent sends a user message with content blocks.
func (p *Process) SendUserContent(content []any, uuid string) error {
	return p.Send(map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil, "uuid": uuid,
	})
}

// Close ends the CLI: closing stdin asks it to exit; it is killed if it
// has not exited soon after.
func (p *Process) Close() {
	_ = p.stdin.Close()
	select {
	case <-p.done:
	default:
		go func() {
			select {
			case <-p.done:
			case <-ctxTimeout():
				_ = p.cmd.Process.Kill()
			}
		}()
	}
}

func ctxTimeout() <-chan time.Time { return time.After(5 * time.Second) }
