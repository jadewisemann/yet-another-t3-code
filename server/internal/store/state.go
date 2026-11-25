package store

import (
	"encoding/json"
	"fmt"
)

// Event types. Their data is the struct of the same name below.
const (
	ProjectCreated        = "project.created"
	ThreadCreated         = "thread.created"
	ThreadMetadataUpdated = "thread.metadata-updated"
	ThreadVisited         = "thread.visited"
	ThreadArchived        = "thread.archived"
	ThreadUnarchived      = "thread.unarchived"
)

type Project struct {
	ID                    string          `json:"id"`
	Title                 string          `json:"title"`
	WorkspaceRoot         string          `json:"workspaceRoot"`
	DefaultModelSelection json.RawMessage `json:"defaultModelSelection,omitempty"`
	Scripts               json.RawMessage `json:"scripts,omitempty"`
	CreatedAt             string          `json:"createdAt"`
	UpdatedAt             string          `json:"updatedAt"`
	ArchivedAt            *string         `json:"archivedAt,omitempty"`
	DeletedAt             *string         `json:"deletedAt,omitempty"`
}

type Thread struct {
	ID              string          `json:"id"`
	ProjectID       string          `json:"projectId"`
	Title           string          `json:"title"`
	CreatedBy       string          `json:"createdBy"`
	CreationSource  string          `json:"creationSource"`
	ModelSelection  json.RawMessage `json:"modelSelection"`
	RuntimeMode     string          `json:"runtimeMode"`
	InteractionMode string          `json:"interactionMode"`
	Branch          *string         `json:"branch,omitempty"`
	WorktreePath    *string         `json:"worktreePath,omitempty"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	ArchivedAt      *string         `json:"archivedAt,omitempty"`
	LastVisitedAt   *string         `json:"lastVisitedAt,omitempty"`

	// Not part of the thread.created data: derived from later events.
	ActiveProviderThreadID *string `json:"-"`
	// TitleRenamed is set once a title is chosen: true for a user rename,
	// false for a generated title. TitleRegeneration is the title request
	// in progress; TitleRegenerationSeen tells whether the thread record
	// carries the key at all (upstream adds it with the first request).
	TitleRenamed          *bool              `json:"-"`
	TitleRegeneration     *TitleRegeneration `json:"-"`
	TitleRegenerationSeen bool               `json:"-"`
	Proj                  *Projection        `json:"-"`
}

type ThreadMetadata struct {
	ThreadID     string  `json:"threadId"`
	Title        *string `json:"title,omitempty"`
	Branch       *string `json:"branch,omitempty"`
	WorktreePath *string `json:"worktreePath,omitempty"`
	// Sending a message also marks the thread visited.
	LastVisitedAt *string `json:"lastVisitedAt,omitempty"`
	TitleRenamed  *bool   `json:"titleRenamed,omitempty"`
	// TitleRegeneration starts a title request; TitleRegenerationDone ends it.
	TitleRegeneration     *TitleRegeneration `json:"titleRegeneration,omitempty"`
	TitleRegenerationDone bool               `json:"titleRegenerationDone,omitempty"`
	At                    string             `json:"at"`
}

type TitleRegeneration struct {
	RequestID string `json:"requestId"`
	StartedAt string `json:"startedAt"`
}

type ThreadRef struct {
	ThreadID string `json:"threadId"`
	At       string `json:"at"`
}

// State is the in-memory projection of the log.
type State struct {
	Projects     map[string]*Project
	ProjectOrder []string
	Threads      map[string]*Thread
	ThreadOrder  []string
}

func newState() *State {
	return &State{Projects: map[string]*Project{}, Threads: map[string]*Thread{}}
}

func (s *State) clone() *State {
	c := newState()
	for k, v := range s.Projects {
		p := *v
		c.Projects[k] = &p
	}
	for k, v := range s.Threads {
		t := *v
		c.Threads[k] = &t
	}
	c.ProjectOrder = append([]string(nil), s.ProjectOrder...)
	c.ThreadOrder = append([]string(nil), s.ThreadOrder...)
	return c
}

// trialApply applies e to a clone made for checking a batch. Thread
// projections are shared with the real state, so v2 events are only checked.
func (s *State) trialApply(e Event) error {
	if IsV2(e.Type) {
		env, err := decodeEnvelope(e)
		if err != nil {
			return err
		}
		_, err = s.thread(env.ThreadID)
		return err
	}
	return s.apply(e)
}

func (s *State) apply(e Event) error {
	if IsV2(e.Type) {
		return s.applyV2(e)
	}
	switch e.Type {
	case ProjectCreated:
		var p Project
		if err := json.Unmarshal(e.Data, &p); err != nil {
			return err
		}
		if _, ok := s.Projects[p.ID]; ok {
			return fmt.Errorf("project %s already exists", p.ID)
		}
		s.Projects[p.ID] = &p
		s.ProjectOrder = append(s.ProjectOrder, p.ID)
	case ThreadCreated:
		var t Thread
		if err := json.Unmarshal(e.Data, &t); err != nil {
			return err
		}
		if _, ok := s.Threads[t.ID]; ok {
			return fmt.Errorf("thread %s already exists", t.ID)
		}
		s.Threads[t.ID] = &t
		s.ThreadOrder = append(s.ThreadOrder, t.ID)
	case ThreadMetadataUpdated:
		var m ThreadMetadata
		if err := json.Unmarshal(e.Data, &m); err != nil {
			return err
		}
		t, err := s.thread(m.ThreadID)
		if err != nil {
			return err
		}
		if m.Title != nil {
			t.Title = *m.Title
		}
		if m.Branch != nil {
			t.Branch = m.Branch
		}
		if m.WorktreePath != nil {
			t.WorktreePath = m.WorktreePath
		}
		if m.LastVisitedAt != nil {
			t.LastVisitedAt = m.LastVisitedAt
		}
		if m.TitleRenamed != nil {
			t.TitleRenamed = m.TitleRenamed
		}
		if m.TitleRegeneration != nil {
			t.TitleRegeneration = m.TitleRegeneration
			t.TitleRegenerationSeen = true
		}
		if m.TitleRegenerationDone {
			t.TitleRegeneration = nil
		}
		t.UpdatedAt = m.At
	case ThreadVisited, ThreadArchived, ThreadUnarchived:
		var r ThreadRef
		if err := json.Unmarshal(e.Data, &r); err != nil {
			return err
		}
		t, err := s.thread(r.ThreadID)
		if err != nil {
			return err
		}
		at := r.At
		switch e.Type {
		case ThreadVisited:
			t.LastVisitedAt = &at
		case ThreadArchived:
			t.ArchivedAt = &at
			t.UpdatedAt = at
		case ThreadUnarchived:
			t.ArchivedAt = nil
			t.UpdatedAt = at
		}
	default:
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	return nil
}

func (s *State) thread(id string) (*Thread, error) {
	t, ok := s.Threads[id]
	if !ok {
		return nil, fmt.Errorf("thread %s does not exist", id)
	}
	return t, nil
}
