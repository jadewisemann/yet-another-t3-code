package conform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/spec"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/wire"
)

// Entry is one line of a transcript.
type Entry struct {
	Step   int    `json:"step"`
	Op     string `json:"op"`
	As     string `json:"as,omitempty"`
	Method string `json:"method,omitempty"`
	// Values are stream chunk values received up to and including the match.
	Values []any `json:"values,omitempty"`
	Exit   any   `json:"exit,omitempty"`
	Status int   `json:"status,omitempty"`
	Body   any   `json:"body,omitempty"`
}

// Result of one scenario run.
type Result struct {
	Transcript []Entry
	// SpecViolations are values the server sent (or accepted) that do not
	// match spec/generated/rpc.json.
	SpecViolations []string
}

type stream struct {
	id     string
	method string
	mu     sync.Mutex
	values []json.RawMessage
	exit   *wire.Exit
	notify chan struct{}
}

// Run executes one scenario against a started target.
func Run(ctx context.Context, sc Scenario, t *Target, sp *spec.Spec) (*Result, error) {
	vars := NewVars(map[string]string{"workspace": t.Workspace, "base": t.BaseDir})
	res := &Result{}
	violate := func(format string, args ...any) {
		res.SpecViolations = append(res.SpecViolations, fmt.Sprintf(format, args...))
	}
	client, err := wire.Dial(ctx, t.WSURL(), t.Token, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer client.Close()

	streams := map[string]*stream{}
	var raws []func(*Normalizer) // deferred normalization, so all generated vars are known
	record := func(build func(n *Normalizer) Entry) {
		raws = append(raws, func(n *Normalizer) { res.Transcript = append(res.Transcript, build(n)) })
	}

	checkExit := func(method string, exit *wire.Exit) {
		if sp == nil || exit == nil {
			return
		}
		if _, inSpec := sp.Methods[method]; !inSpec {
			return
		}
		switch exit.Tag {
		case "Success":
			if sp.Methods[method].Kind == "unary" {
				if err := sp.Validate(method, spec.Success, exit.Value); err != nil {
					violate("%s success: %v", method, err)
				}
			}
		case "Failure":
			for _, c := range exit.Cause {
				if c.Tag == "Fail" {
					if err := sp.Validate(method, spec.Error, c.Error); err != nil {
						violate("%s error: %v", method, err)
					}
				}
			}
		}
	}

	for i, st := range sc.Steps {
		timeout := time.Duration(st.TimeoutMs) * time.Millisecond
		if timeout == 0 {
			timeout = 15 * time.Second
		}
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		fail := func(err error) (*Result, error) {
			cancel()
			return res, fmt.Errorf("step %d (%s %s%s): %w", i, st.Op, st.Method, st.Stream, err)
		}
		step := i
		switch st.Op {
		case "call", "subscribe":
			payload, err := vars.Expand(orEmpty(st.Payload))
			if err != nil {
				return fail(err)
			}
			if _, inSpec := sp.Methods[st.Method]; sp != nil && inSpec {
				if err := sp.Validate(st.Method, spec.Payload, payload); err != nil {
					violate("step %d sends a payload the spec rejects: %v", i, err)
				}
			}
			if st.Op == "call" {
				exit, err := client.Call(stepCtx, st.Method, payload)
				if err != nil {
					return fail(err)
				}
				checkExit(st.Method, exit)
				exitRaw, _ := json.Marshal(exit)
				st := st
				record(func(n *Normalizer) Entry {
					return Entry{Step: step, Op: "call", As: st.As, Method: st.Method, Exit: n.Value(exitRaw)}
				})
			} else {
				id, ch, err := client.Start(st.Method, payload)
				if err != nil {
					return fail(err)
				}
				s := &stream{id: id, method: st.Method, notify: make(chan struct{}, 1)}
				streams[st.As] = s
				go func(method string) {
					for f := range ch {
						s.mu.Lock()
						if f.Tag == wire.TagChunk {
							for _, v := range f.Values {
								if sp != nil {
									if err := sp.Validate(method, spec.Success, v); err != nil {
										s.mu.Unlock()
										violate("%s chunk: %v", method, err)
										s.mu.Lock()
									}
								}
							}
							s.values = append(s.values, f.Values...)
						} else if f.Tag == wire.TagExit {
							s.exit = f.Exit
						}
						s.mu.Unlock()
						select {
						case s.notify <- struct{}{}:
						default:
						}
					}
				}(st.Method)
				st := st
				record(func(n *Normalizer) Entry { return Entry{Step: step, Op: "subscribe", As: st.As, Method: st.Method} })
			}
		case "await":
			s := streams[st.Stream]
			if s == nil {
				return fail(fmt.Errorf("no stream named %q", st.Stream))
			}
			var want any
			if err := json.Unmarshal(orEmpty(st.Match), &want); err != nil {
				return fail(err)
			}
			got, err := s.takeUntil(stepCtx, want)
			if err != nil {
				return fail(err)
			}
			st := st
			record(func(n *Normalizer) Entry {
				vals := make([]any, len(got))
				for j, g := range got {
					vals[j] = n.Value(g)
				}
				return Entry{Step: step, Op: "await", As: st.Stream, Values: vals}
			})
		case "interrupt":
			s := streams[st.Stream]
			if s == nil {
				return fail(fmt.Errorf("no stream named %q", st.Stream))
			}
			if err := client.Interrupt(s.id); err != nil {
				return fail(err)
			}
			exit, err := s.waitExit(stepCtx)
			if err != nil {
				return fail(err)
			}
			exitRaw, _ := json.Marshal(exit)
			st := st
			record(func(n *Normalizer) Entry {
				return Entry{Step: step, Op: "interrupt", As: st.Stream, Exit: n.Value(exitRaw)}
			})
		case "http":
			req, err := http.NewRequestWithContext(stepCtx, defaultStr(st.HTTPMethod, "GET"), t.Origin()+st.Path, nil)
			if err != nil {
				return fail(err)
			}
			if st.Auth {
				req.Header.Set("Authorization", "Bearer "+t.Token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fail(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			status := resp.StatusCode
			st := st
			record(func(n *Normalizer) Entry {
				e := Entry{Step: step, Op: "http", As: st.Path, Status: status}
				switch st.Record {
				case "status":
				case "keys":
					var m map[string]json.RawMessage
					if json.Unmarshal(body, &m) == nil {
						keys := make([]string, 0, len(m))
						for k := range m {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						e.Body = keys
					}
				default:
					e.Body = n.Value(body)
				}
				return e
			})
		default:
			return fail(fmt.Errorf("unknown op"))
		}
		cancel()
	}
	n := NewNormalizer(vars)
	for _, f := range raws {
		f(n)
	}
	return res, nil
}

// takeUntil returns the values received since the last call, up to and
// including the first one that matches want.
func (s *stream) takeUntil(ctx context.Context, want any) ([]json.RawMessage, error) {
	for {
		s.mu.Lock()
		for i, v := range s.values {
			var got any
			_ = json.Unmarshal(v, &got)
			if matches(want, got) {
				out := append([]json.RawMessage(nil), s.values[:i+1]...)
				s.values = s.values[i+1:]
				s.mu.Unlock()
				return out, nil
			}
		}
		exit := s.exit
		pending := len(s.values)
		var seen []string
		for _, v := range s.values {
			t := string(v)
			if len(t) > 200 {
				t = t[:200] + "…"
			}
			seen = append(seen, t)
		}
		s.mu.Unlock()
		if exit != nil {
			raw, _ := json.Marshal(exit)
			return nil, fmt.Errorf("stream ended before a match: %s", raw)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no matching value after %d values: %w\n%s", pending, ctx.Err(), strings.Join(seen, "\n"))
		case <-s.notify:
		}
	}
}

func (s *stream) waitExit(ctx context.Context) (*wire.Exit, error) {
	for {
		s.mu.Lock()
		exit := s.exit
		s.mu.Unlock()
		if exit != nil {
			return exit, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("stream did not end: %w", ctx.Err())
		case <-s.notify:
		}
	}
}

// matches reports whether every key of want is in got with a matching value.
func matches(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			if !matches(wv, g[k]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
