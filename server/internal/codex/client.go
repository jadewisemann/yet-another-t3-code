// Package codex talks to `codex app-server`: JSON-RPC 2.0 without the
// "jsonrpc" member, one JSON object per line over the child's stdio. The
// methods and their schemas are in spec/generated/codex.json.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// Notification is a message from codex without an id, or a request from
// codex (ID set) that the client must answer with Respond.
type Notification struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// RPCError is an error response from codex.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("codex: %s (%d)", e.Message, e.Code) }

// Client is one running app-server process.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	mu     sync.Mutex
	nextID int64
	calls  map[int64]chan response
	subs   map[int]chan Notification
	nsub   int
	done   chan struct{}
	err    error
}

type response struct {
	result json.RawMessage
	err    error
}

// Start runs `<bin> app-server` and reads its output until it exits.
func Start(bin, dir string, env []string, stderr io.Writer) (*Client, error) {
	cmd := exec.Command(bin, "app-server")
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
	c := &Client{cmd: cmd, stdin: stdin, calls: map[int64]chan response{}, subs: map[int]chan Notification{}, done: make(chan struct{})}
	go c.read(stdout)
	return c, nil
}

func (c *Client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Method != "" {
			c.mu.Lock()
			for _, ch := range c.subs {
				ch <- Notification{ID: m.ID, Method: m.Method, Params: m.Params}
			}
			c.mu.Unlock()
			continue
		}
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.calls[id]
		delete(c.calls, id)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		if m.Error != nil {
			ch <- response{err: m.Error}
		} else {
			ch <- response{result: m.Result}
		}
	}
	err := c.cmd.Wait()
	if err == nil {
		err = errors.New("codex app-server exited")
	}
	c.mu.Lock()
	c.err = fmt.Errorf("codex app-server: %w", err)
	for id, ch := range c.calls {
		ch <- response{err: c.err}
		delete(c.calls, id)
	}
	for id, ch := range c.subs {
		close(ch)
		delete(c.subs, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

// Call sends a request and waits for its result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.calls[id] = ch
	c.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

// Respond answers a request codex sent.
func (c *Client) Respond(id json.RawMessage, result any, rpcErr *RPCError) error {
	m := map[string]any{"id": id}
	if rpcErr != nil {
		m["error"] = rpcErr
	} else {
		m["result"] = result
	}
	return c.write(m)
}

// Subscribe delivers every later notification and server request until
// cancel is called or the process exits (the channel is then closed).
func (c *Client) Subscribe() (<-chan Notification, func()) {
	ch := make(chan Notification, 1024)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		close(ch)
		return ch, func() {}
	}
	id := c.nsub
	c.nsub++
	c.subs[id] = ch
	return ch, func() {
		c.mu.Lock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(ch)
		}
		c.mu.Unlock()
	}
}

// Done is closed when the process has exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close ends the process.
func (c *Client) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.done
	return nil
}

// Initialize performs the handshake upstream T3 Code does for a turn session.
func (c *Client) Initialize(ctx context.Context, version string) (json.RawMessage, error) {
	res, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "T3 Code", "title": "T3 Code", "version": version},
		"capabilities": map[string]any{
			"experimentalApi":           true,
			"optOutNotificationMethods": []string{"turn/diff/updated"},
		},
	})
	if err != nil {
		return nil, err
	}
	return res, c.Notify("initialized", nil)
}
