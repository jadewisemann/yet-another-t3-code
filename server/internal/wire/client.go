package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

// Client is a minimal Effect RPC client. It acknowledges every chunk, as the
// server waits for an Ack before sending the next one.
type Client struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	nextID int
	subs   map[string]chan Frame
	done   chan struct{}
	err    error
	// Raw receives every frame the server sends, in order, if set before Dial returns.
	Raw func(Frame)
}

// Dial opens `/ws` on origin with a bearer token.
func Dial(ctx context.Context, wsURL, token string, raw func(Frame)) (*Client, error) {
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	c := &Client{conn: conn, subs: map[string]chan Frame{}, done: make(chan struct{}), Raw: raw}
	go c.read()
	return c, nil
}

func (c *Client) read() {
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			for _, ch := range c.subs {
				close(ch)
			}
			c.subs = map[string]chan Frame{}
			c.mu.Unlock()
			return
		}
		f, err := Decode(data)
		if err != nil {
			continue
		}
		if c.Raw != nil {
			c.Raw(f)
		}
		if f.Tag == TagChunk {
			_ = c.Send(Frame{Tag: TagAck, RequestID: f.RequestID})
		}
		if len(f.RequestID) == 0 {
			continue
		}
		key := IDString(f.RequestID)
		c.mu.Lock()
		ch := c.subs[key]
		if f.Tag == TagExit {
			delete(c.subs, key)
		}
		c.mu.Unlock()
		if ch != nil {
			ch <- f
			if f.Tag == TagExit {
				close(ch)
			}
		}
	}
}

// Send writes one frame.
func (c *Client) Send(f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageText, data)
}

// Start sends a request and returns its id and a channel of the frames that
// answer it (chunks, then one Exit). The channel closes after the Exit.
func (c *Client) Start(method string, payload json.RawMessage) (string, <-chan Frame, error) {
	c.mu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan Frame, 1024)
	c.subs[id] = ch
	c.mu.Unlock()
	rawID, _ := json.Marshal(id)
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	err := c.Send(Frame{Tag: TagRequest, ID: rawID, Method: method, Payload: payload, Headers: &[][2]string{}})
	return id, ch, err
}

// Interrupt ends a stream.
func (c *Client) Interrupt(id string) error {
	rawID, _ := json.Marshal(id)
	return c.Send(Frame{Tag: TagInterrupt, RequestID: rawID, Interruptors: []int{}})
}

// Call sends a request and waits for its Exit.
func (c *Client) Call(ctx context.Context, method string, payload json.RawMessage) (*Exit, error) {
	_, ch, err := c.Start(method, payload)
	if err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case f, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("connection closed: %v", c.err)
			}
			if f.Tag == TagExit {
				return f.Exit, nil
			}
		}
	}
}

// Close closes the socket.
func (c *Client) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}
