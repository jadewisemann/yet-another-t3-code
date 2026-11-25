package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Orchestration v2 events are stored as upstream sends them on the thread
// stream: an envelope with the full entity in "payload". Folding one is an
// upsert by id into the thread's projection, as upstream's applyToProjection
// does (apps/server/src/orchestration-v2/ProjectionStore.ts).

// Envelope is the v2 event as the thread stream sends it (without sequence).
type Envelope struct {
	ID                 string          `json:"id"`
	ThreadID           string          `json:"threadId"`
	RunID              string          `json:"runId,omitempty"`
	NodeID             string          `json:"nodeId,omitempty"`
	Driver             string          `json:"driver,omitempty"`
	ProviderInstanceID string          `json:"providerInstanceId,omitempty"`
	OccurredAt         string          `json:"occurredAt"`
	Type               string          `json:"type"`
	Payload            json.RawMessage `json:"payload"`
}

// collections maps each v2 event type to the projection list it upserts into.
var collections = map[string]string{
	"run.created":               "runs",
	"run.updated":               "runs",
	"run-attempt.created":       "attempts",
	"run-attempt.updated":       "attempts",
	"node.updated":              "nodes",
	"subagent.updated":          "subagents",
	"provider-session.attached": "providerSessions",
	"provider-session.updated":  "providerSessions",
	"provider-thread.updated":   "providerThreads",
	"provider-turn.updated":     "providerTurns",
	"runtime-request.updated":   "runtimeRequests",
	"message.updated":           "messages",
	"turn-item.updated":         "turnItems",
	"plan.updated":              "plans",
	"checkpoint-scope.created":  "checkpointScopes",
	"checkpoint.captured":       "checkpoints",
	"context-handoff.updated":   "contextHandoffs",
	"context-transfer.created":  "contextTransfers",
	"context-transfer.updated":  "contextTransfers",
}

// Collections lists the projection lists in the order the snapshot names them.
var Collections = []string{
	"runs", "attempts", "nodes", "subagents", "providerSessions", "providerThreads",
	"providerTurns", "runtimeRequests", "messages", "plans", "turnItems",
	"checkpointScopes", "checkpoints", "contextHandoffs", "contextTransfers",
}

// IsV2 reports whether an event type is a v2 entity event.
func IsV2(typ string) bool { _, ok := collections[typ]; return ok }

// Entity is one projected record: its payload and the fields lists sort by.
type Entity struct {
	ID      string
	Payload json.RawMessage
	keys    map[string]any
}

// Field returns a top-level payload field (decoded lazily).
func (e *Entity) Field(name string) any {
	if e.keys == nil {
		_ = json.Unmarshal(e.Payload, &e.keys)
	}
	return e.keys[name]
}

// String returns a string field, or "" when it is missing or not a string.
func (e *Entity) String(name string) string {
	s, _ := e.Field(name).(string)
	return s
}

// Number returns a numeric field, or 0.
func (e *Entity) Number(name string) float64 {
	f, _ := e.Field(name).(float64)
	return f
}

// Projection is a thread's v2 entities by list name and id.
type Projection struct {
	lists     map[string]map[string]*Entity
	UpdatedAt string
}

func newProjection() *Projection {
	return &Projection{lists: map[string]map[string]*Entity{}}
}

// Get returns the entity with this id in a list, or nil.
func (p *Projection) Get(list, id string) *Entity {
	if p == nil {
		return nil
	}
	return p.lists[list][id]
}

// List returns a list's entities in the order upstream's snapshot query uses.
func (p *Projection) List(list string) []*Entity {
	if p == nil {
		return nil
	}
	out := make([]*Entity, 0, len(p.lists[list]))
	for _, e := range p.lists[list] {
		out = append(out, e)
	}
	str := func(e *Entity, k string) string { return e.String(k) }
	num := func(e *Entity, k string) float64 { return e.Number(k) }
	byID := func(a, b *Entity) bool { return a.ID < b.ID }
	var less func(a, b *Entity) bool
	switch list {
	case "runs":
		less = func(a, b *Entity) bool { return num(a, "ordinal") < num(b, "ordinal") }
	case "attempts":
		less = func(a, b *Entity) bool {
			if str(a, "runId") != str(b, "runId") {
				return str(a, "runId") < str(b, "runId")
			}
			return num(a, "attemptOrdinal") < num(b, "attemptOrdinal")
		}
	case "nodes", "subagents":
		less = func(a, b *Entity) bool {
			if str(a, "startedAt") != str(b, "startedAt") {
				return str(a, "startedAt") < str(b, "startedAt")
			}
			return byID(a, b)
		}
	case "providerSessions":
		less = func(a, b *Entity) bool {
			if str(a, "updatedAt") != str(b, "updatedAt") {
				return str(a, "updatedAt") < str(b, "updatedAt")
			}
			return byID(a, b)
		}
	case "providerThreads":
		less = func(a, b *Entity) bool {
			if num(a, "firstRunOrdinal") != num(b, "firstRunOrdinal") {
				return num(a, "firstRunOrdinal") < num(b, "firstRunOrdinal")
			}
			return byID(a, b)
		}
	case "providerTurns":
		less = func(a, b *Entity) bool {
			if str(a, "providerThreadId") != str(b, "providerThreadId") {
				return str(a, "providerThreadId") < str(b, "providerThreadId")
			}
			return num(a, "ordinal") < num(b, "ordinal")
		}
	case "runtimeRequests", "messages":
		less = func(a, b *Entity) bool {
			if str(a, "createdAt") != str(b, "createdAt") {
				return str(a, "createdAt") < str(b, "createdAt")
			}
			return byID(a, b)
		}
	case "turnItems":
		less = func(a, b *Entity) bool {
			if num(a, "ordinal") != num(b, "ordinal") {
				return num(a, "ordinal") < num(b, "ordinal")
			}
			return byID(a, b)
		}
	case "checkpoints":
		less = func(a, b *Entity) bool {
			if str(a, "scopeId") != str(b, "scopeId") {
				return str(a, "scopeId") < str(b, "scopeId")
			}
			return num(a, "ordinalWithinScope") < num(b, "ordinalWithinScope")
		}
	default:
		less = byID
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

func (p *Projection) upsert(list string, payload json.RawMessage) error {
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &ref); err != nil || ref.ID == "" {
		return fmt.Errorf("%s payload without id", list)
	}
	if p.lists[list] == nil {
		p.lists[list] = map[string]*Entity{}
	}
	p.lists[list][ref.ID] = &Entity{ID: ref.ID, Payload: payload}
	return nil
}

func decodeEnvelope(e Event) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(e.Data, &env); err != nil {
		return env, err
	}
	if env.ThreadID == "" {
		return env, fmt.Errorf("%s event without threadId", e.Type)
	}
	return env, nil
}

// applyV2 folds a v2 event into its thread.
func (s *State) applyV2(e Event) error {
	env, err := decodeEnvelope(e)
	if err != nil {
		return err
	}
	t, err := s.thread(env.ThreadID)
	if err != nil {
		return err
	}
	if t.Proj == nil {
		t.Proj = newProjection()
	}
	list := collections[e.Type]
	if err := t.Proj.upsert(list, env.Payload); err != nil {
		return err
	}
	t.Proj.UpdatedAt = env.OccurredAt
	t.UpdatedAt = env.OccurredAt
	if e.Type == "provider-thread.updated" {
		var pt struct {
			ID          string `json:"id"`
			AppThreadID string `json:"appThreadId"`
		}
		_ = json.Unmarshal(env.Payload, &pt)
		if pt.AppThreadID == t.ID && !strings.HasPrefix(pt.ID, "provider-thread:queued:") {
			id := pt.ID
			t.ActiveProviderThreadID = &id
		}
	}
	return nil
}
