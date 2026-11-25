// Package server is the Go implementation of the T3 Code server.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/wire"
)

// Version is the upstream T3 Code version whose behavior this server
// implements (spec/upstream-version). Clients see it as `serverVersion`,
// because they use it to decide what the server speaks.
var Version = "0.1.11"

// BuildVersion is yatt-server's own version, shown by `--version`.
var BuildVersion = "0.0.0-dev"

// OrchestrationProtocolVersion is what `/ws?orchestrationProtocol=` must carry.
const OrchestrationProtocolVersion = 2

// Config is what `serve` needs.
type Config struct {
	BaseDir string
	Host    string
	Port    int
	Cwd     string
}

// Server holds the running state.
type Server struct {
	cfg      Config
	sessions *Sessions
	envID    string
	handlers map[string]Handler
	log      *slog.Logger
	store    *store.Store
	runs     *runner
}

// Handler answers one RPC method. Unary handlers return a value or an error;
// stream handlers call emit for each value and return when done.
type Handler struct {
	Stream bool
	Unary  func(ctx context.Context, payload json.RawMessage) (any, error)
	Run    func(ctx context.Context, payload json.RawMessage, emit func(values ...any) error) error
}

// TypedError is a failure with a `_tag`, sent as a `Fail` cause. Fields
// holds the rest of the error object (message, commandId, ...).
type TypedError struct {
	Tag    string
	Fields map[string]any
}

func (e *TypedError) Error() string { return fmt.Sprintf("%s: %v", e.Tag, e.Fields["message"]) }

func (e *TypedError) MarshalJSON() ([]byte, error) {
	m := map[string]any{"_tag": e.Tag}
	for k, v := range e.Fields {
		m[k] = v
	}
	return json.Marshal(m)
}

func UserData(baseDir string) string { return filepath.Join(baseDir, "userdata") }

func New(cfg Config) (*Server, error) {
	ud := UserData(cfg.BaseDir)
	if err := os.MkdirAll(ud, 0o700); err != nil {
		return nil, err
	}
	envID, err := loadOrCreateID(filepath.Join(ud, "environment-id"))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ud)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, sessions: NewSessions(ud), envID: envID, log: slog.Default(), store: st}
	s.runs = newRunner(s)
	st.Render = renderThreadEvent
	s.handlers = s.registerHandlers()
	return s, nil
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func loadOrCreateID(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	return id, os.WriteFile(path, []byte(id), 0o600)
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/t3/environment", s.environment)
	mux.HandleFunc("GET /ws", s.websocket)
	mux.HandleFunc("POST /oauth/token", s.oauthToken)
	return mux
}

func (s *Server) environment(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, 200, map[string]any{
		"environmentId":                s.envID,
		"label":                        host,
		"platform":                     map[string]string{"os": platformOS(), "arch": goArch()},
		"serverVersion":                Version,
		"orchestrationProtocolVersion": OrchestrationProtocolVersion,
		"databaseMigration":            0,
		"capabilities":                 map[string]any{},
	})
}

func goArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return "other"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("orchestrationProtocol") != fmt.Sprint(OrchestrationProtocolVersion) {
		writeJSON(w, http.StatusUpgradeRequired, map[string]any{
			"code":                         "orchestration_protocol_incompatible",
			"message":                      fmt.Sprintf("Update this client to one that supports orchestration protocol %d.", OrchestrationProtocolVersion),
			"orchestrationProtocolVersion": OrchestrationProtocolVersion,
		})
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.sessions.Valid(token) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	c := &connection{s: s, conn: conn, pending: map[string]*pending{}}
	c.serve(r.Context())
}

// connection runs the RPC protocol for one socket. See spec/protocol/wire.md.
type connection struct {
	s       *Server
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]*pending
	fiber   int
}

type pending struct {
	cancel context.CancelFunc
	acks   chan struct{}
}

func (c *connection) send(f wire.Frame) {
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.Write(context.Background(), websocket.MessageText, data)
}

func (c *connection) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		f, err := wire.Decode(data)
		if err != nil {
			continue
		}
		switch f.Tag {
		case wire.TagPing:
			c.send(wire.Frame{Tag: wire.TagPong})
		case wire.TagRequest:
			c.start(ctx, f)
		case wire.TagAck:
			c.mu.Lock()
			p := c.pending[wire.IDString(f.RequestID)]
			c.mu.Unlock()
			if p != nil {
				select {
				case p.acks <- struct{}{}:
				default:
				}
			}
		case wire.TagInterrupt:
			c.mu.Lock()
			p := c.pending[wire.IDString(f.RequestID)]
			c.mu.Unlock()
			if p != nil {
				p.cancel()
			}
		case wire.TagEOF:
			return
		}
	}
}

func (c *connection) start(parent context.Context, req wire.Frame) {
	h, ok := c.s.handlers[req.Method]
	if !ok {
		c.send(wire.Die(req.ID, "Unknown request tag: "+req.Method))
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p := &pending{cancel: cancel, acks: make(chan struct{}, 1)}
	key := wire.IDString(req.ID)
	c.mu.Lock()
	c.pending[key] = p
	c.fiber++
	fiber := c.fiber
	c.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.pending, key)
			c.mu.Unlock()
		}()
		var err error
		if h.Stream {
			err = h.Run(ctx, req.Payload, func(values ...any) error {
				f, err := wire.Chunk(req.ID, values...)
				if err != nil {
					return err
				}
				c.send(f)
				// The client acknowledges each chunk before the next one.
				select {
				case <-p.acks:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err == nil {
				// A stream that ends on its own; upstream streams stay open until interrupted.
				<-ctx.Done()
				err = ctx.Err()
			}
		} else {
			var v any
			v, err = h.Unary(ctx, req.Payload)
			if err == nil {
				f, merr := wire.Success(req.ID, v)
				if merr != nil {
					c.send(wire.Die(req.ID, merr.Error()))
				} else {
					c.send(f)
				}
				return
			}
		}
		var typed *TypedError
		switch {
		case ctx.Err() != nil:
			c.send(wire.Frame{Tag: wire.TagExit, RequestID: req.ID, Exit: &wire.Exit{
				Tag: "Failure", Cause: []wire.Cause{{Tag: "Interrupt", FiberID: &fiber}},
			}})
		case asTyped(err, &typed):
			f, _ := wire.Fail(req.ID, typed)
			c.send(f)
		default:
			c.send(wire.Die(req.ID, err.Error()))
		}
	}()
}

func asTyped(err error, out **TypedError) bool {
	t, ok := err.(*TypedError)
	if ok {
		*out = t
	}
	return ok
}
