package conform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
	aside  []json.RawMessage // values that matched an aside pattern
	asideP []any
	exit   *wire.Exit
	notify chan struct{}
}

// Run executes one scenario against a started target.
func Run(ctx context.Context, sc Scenario, t *Target, sp *spec.Spec) (*Result, error) {
	vars := NewVars(map[string]string{"workspace": t.Workspace, "base": t.BaseDir, "origin": t.Origin(), "port": fmt.Sprint(t.Port)})
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
				if exit.Tag == "Success" {
					if err := capture(vars, st.Capture, exit.Value); err != nil {
						return fail(err)
					}
				}
				exitRaw, _ := json.Marshal(exit)
				st := st
				record(func(n *Normalizer) Entry {
					if st.Record == "keys" && exit.Tag == "Success" {
						return Entry{Step: step, Op: "call", As: st.As, Method: st.Method, Exit: map[string]any{"_tag": "Success", "valueKeys": shape(n, exit.Value, "keys")}}
					}
					return Entry{Step: step, Op: "call", As: st.As, Method: st.Method, Exit: n.Value(exitRaw)}
				})
			} else {
				id, ch, err := client.Start(st.Method, payload)
				if err != nil {
					return fail(err)
				}
				s := &stream{id: id, method: st.Method, notify: make(chan struct{}, 1)}
				for _, raw := range st.Aside {
					var p any
					if err := json.Unmarshal(raw, &p); err != nil {
						return fail(err)
					}
					s.asideP = append(s.asideP, p)
				}
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
							for _, v := range f.Values {
								var got any
								_ = json.Unmarshal(v, &got)
								set := false
								for _, p := range s.asideP {
									if matches(p, got) {
										set = true
									}
								}
								if set {
									s.aside = append(s.aside, v)
								} else {
									s.values = append(s.values, v)
								}
							}
							trace(method, f.Values)
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
			got, err := s.takeUntil(stepCtx, want, st.FromAside)
			if err != nil {
				return fail(err)
			}
			if len(got) > 0 {
				if err := capture(vars, st.Capture, got[len(got)-1]); err != nil {
					return fail(err)
				}
			}
			if st.Record == "match" {
				got = got[len(got)-1:]
			}
			st := st
			record(func(n *Normalizer) Entry {
				vals := make([]any, len(got))
				for j, g := range got {
					vals[j] = ignoreFields(n.Value(g), st.Ignore)
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
			var reqBody io.Reader
			if st.Body != "" {
				b, err := vars.Expand(json.RawMessage(st.Body))
				if err != nil {
					return fail(err)
				}
				reqBody = strings.NewReader(string(b))
			}
			req, err := http.NewRequestWithContext(stepCtx, defaultStr(st.HTTPMethod, "GET"), t.Origin()+st.Path, reqBody)
			if err != nil {
				return fail(err)
			}
			if st.ContentType != "" {
				req.Header.Set("Content-Type", st.ContentType)
			}
			if st.Auth {
				req.Header.Set("Authorization", "Bearer "+t.Token)
			}
			if st.Bearer != "" {
				tok, err := vars.Expand(json.RawMessage(st.Bearer))
				if err != nil {
					return fail(err)
				}
				req.Header.Set("Authorization", "Bearer "+string(tok))
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fail(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err := capture(vars, st.Capture, body); err != nil {
				return fail(err)
			}
			status := resp.StatusCode
			st := st
			record(func(n *Normalizer) Entry {
				return Entry{Step: step, Op: "http", As: st.Path, Status: status, Body: shape(n, body, st.Record)}
			})
		case "cli":
			args := []string{}
			for _, a := range st.Args {
				x, err := vars.Expand(json.RawMessage(a))
				if err != nil {
					return fail(err)
				}
				args = append(args, string(x))
			}
			args = append(args, "--base-dir", t.BaseDir)
			cmd := exec.CommandContext(stepCtx, t.Bin, args...)
			cmd.Dir = t.Workspace
			out, err := cmd.Output()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				return fail(err)
			}
			if err := capture(vars, st.Capture, out); err != nil {
				return fail(err)
			}
			st := st
			record(func(n *Normalizer) Entry {
				return Entry{Step: step, Op: "cli", As: strings.Join(st.Args, " "), Status: code, Body: shape(n, out, st.Record)}
			})
		case "connect":
			// Opens a second socket with another token and reports whether
			// the upgrade succeeded (101) or the status it was refused with.
			tok, err := vars.Expand(json.RawMessage(st.Bearer))
			if err != nil {
				return fail(err)
			}
			status := 101
			c, err := wire.Dial(stepCtx, t.WSURL(), string(tok), nil)
			if err != nil {
				status = 0
				if m := statusRe.FindStringSubmatch(err.Error()); m != nil {
					fmt.Sscan(m[1], &status)
				}
			} else {
				c.Close()
			}
			record(func(n *Normalizer) Entry { return Entry{Step: step, Op: "connect", Status: status} })
		case "file":
			data, err := os.ReadFile(filepath.Join(t.BaseDir, st.File))
			if err != nil {
				return fail(err)
			}
			st := st
			record(func(n *Normalizer) Entry {
				return Entry{Step: step, Op: "file", As: st.File, Body: shape(n, data, st.Record)}
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

// ignoreFields replaces the values of the named fields, at any depth.
func ignoreFields(v any, names []string) any {
	if len(names) == 0 {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if contains(names, k) {
				x[k] = "<ignored>"
			} else {
				x[k] = ignoreFields(e, names)
			}
		}
	case []any:
		for i, e := range x {
			x[i] = ignoreFields(e, names)
		}
	}
	return v
}

var statusRe = regexp.MustCompile(`status code 101 but got (\d+)`)

// shape picks the part of a JSON result the transcript keeps.
func shape(n *Normalizer, raw []byte, mode string) any {
	switch mode {
	case "status":
		return nil
	case "keys":
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return "<not a JSON object>"
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	default:
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil
		}
		if !json.Valid(raw) {
			return n.str(strings.TrimSpace(string(raw)))
		}
		return n.Value(raw)
	}
}

// capture stores string fields of a JSON object as variables. A field
// may be a dotted path such as "event.payload.requestId".
func capture(v *Vars, fields map[string]string, raw []byte) error {
	if len(fields) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("capturing from a result that is not a JSON object: %s", truncate(string(raw), 200))
	}
	for name, field := range fields {
		var cur any = m
		for _, part := range strings.Split(field, ".") {
			obj, _ := cur.(map[string]any)
			cur = obj[part]
		}
		val, ok := cur.(string)
		if !ok {
			return fmt.Errorf("result has no string field %q to capture: %s", field, truncate(string(raw), 200))
		}
		v.Set(name, val)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// takeUntil returns the values received since the last call, up to and
// including the first one that matches want.
func (s *stream) takeUntil(ctx context.Context, want any, fromAside bool) ([]json.RawMessage, error) {
	for {
		s.mu.Lock()
		list := &s.values
		if fromAside {
			list = &s.aside
		}
		for i, v := range *list {
			var got any
			_ = json.Unmarshal(v, &got)
			if matches(want, got) {
				out := append([]json.RawMessage(nil), (*list)[:i+1]...)
				*list = (*list)[i+1:]
				s.mu.Unlock()
				return out, nil
			}
		}
		exit := s.exit
		pending := len(*list)
		var seen []string
		for _, v := range *list {
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
// matches reports whether got has every key of want with an equal value.
// The string "$present" matches any value that is there.
func matches(want, got any) bool {
	if want == "$present" {
		return got != nil
	}
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

// trace appends stream values to $T3CONFORM_TRACE, for writing scenarios.
func trace(method string, values []json.RawMessage) {
	path := os.Getenv("T3CONFORM_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, v := range values {
		fmt.Fprintf(f, "%s\t%s\n", method, v)
	}
}
