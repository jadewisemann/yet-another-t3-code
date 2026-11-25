package server

import (
	"context"
	"encoding/json"
	"sort"
)

// registerHandlers lists the methods this server implements. Every other
// method in spec/generated/rpc.json answers "Unknown request tag", the same
// defect upstream sends for a method it does not have; spec/coverage.md
// tracks which phase implements which method.
func (s *Server) registerHandlers() map[string]Handler {
	return map[string]Handler{
		"orchestration.subscribeShell": {Stream: true, Run: s.subscribeShell},
	}
}

// ImplementedMethods lists the RPC methods this server answers, sorted.
func ImplementedMethods() []string {
	var s Server
	out := make([]string, 0)
	for m := range s.registerHandlers() {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

type shellSubscribeInput struct {
	AfterSequence           *int64 `json:"afterSequence"`
	RequestCompletionMarker bool   `json:"requestCompletionMarker"`
}

// subscribeShell is a placeholder that always reports an empty shell. The
// real one (phase 1) reads the projection store and follows its changes.
func (s *Server) subscribeShell(ctx context.Context, payload json.RawMessage, emit func(...any) error) error {
	var in shellSubscribeInput
	_ = json.Unmarshal(payload, &in)
	snapshot := map[string]any{
		"kind": "snapshot",
		"snapshot": map[string]any{
			"schemaVersion":    2,
			"snapshotSequence": 0,
			"threads":          []any{},
			"archivedThreads":  []any{},
			"projects":         []any{},
		},
	}
	values := []any{snapshot}
	if in.RequestCompletionMarker {
		values = append(values, map[string]any{"kind": "synchronized"})
	}
	for _, v := range values {
		if err := emit(v); err != nil {
			return err
		}
	}
	return nil
}
