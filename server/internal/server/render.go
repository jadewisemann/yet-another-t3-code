package server

import (
	"encoding/json"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// The functions here turn projections into the wire shapes recorded in
// conformance/golden. Fields yatt-server does not model yet are sent with
// the value upstream uses for a thread or project that never used them.

func rawOr(v json.RawMessage, fallback any) any {
	if len(v) == 0 || string(v) == "null" {
		return fallback
	}
	return v
}

func ptr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func projectBase(p *store.Project) map[string]any {
	return map[string]any{
		"id":                    p.ID,
		"title":                 p.Title,
		"workspaceRoot":         p.WorkspaceRoot,
		"defaultModelSelection": rawOr(p.DefaultModelSelection, nil),
		"scripts":               rawOr(p.Scripts, []any{}),
		"createdAt":             p.CreatedAt,
		"updatedAt":             p.UpdatedAt,
		"archivedAt":            ptr(p.ArchivedAt),
		"autoPull":              false,
		"defaultThreadEnvMode":  nil,
		"faviconPath":           nil,
		"pinnedAt":              nil,
		"projectIcon":           nil,
		"repositoryIdentity":    nil,
	}
}

// renderProject is the project as projects.mutate returns it.
func renderProject(p *store.Project) map[string]any {
	m := projectBase(p)
	m["deletedAt"] = ptr(p.DeletedAt)
	return m
}

// renderShellProject is the project as the shell stream sends it.
func renderShellProject(p *store.Project) map[string]any {
	m := projectBase(p)
	m["hasCustomTitle"] = false
	return m
}

func providerInstanceID(t *store.Thread) any {
	var sel struct {
		InstanceID *string `json:"instanceId"`
		Provider   *string `json:"provider"`
	}
	_ = json.Unmarshal(t.ModelSelection, &sel)
	if sel.InstanceID != nil {
		return *sel.InstanceID
	}
	if sel.Provider != nil {
		return *sel.Provider
	}
	return nil
}

func threadCommon(t *store.Thread) map[string]any {
	return map[string]any{
		"id":                     t.ID,
		"projectId":              t.ProjectID,
		"title":                  t.Title,
		"createdBy":              t.CreatedBy,
		"creationSource":         t.CreationSource,
		"modelSelection":         rawOr(t.ModelSelection, nil),
		"providerInstanceId":     providerInstanceID(t),
		"runtimeMode":            t.RuntimeMode,
		"interactionMode":        t.InteractionMode,
		"branch":                 ptr(t.Branch),
		"worktreePath":           ptr(t.WorktreePath),
		"createdAt":              t.CreatedAt,
		"updatedAt":              t.UpdatedAt,
		"archivedAt":             ptr(t.ArchivedAt),
		"lastVisitedAt":          ptr(t.LastVisitedAt),
		"deletedAt":              nil,
		"forkedFrom":             nil,
		"activeProviderThreadId": ptr(t.ActiveProviderThreadID),
		"lineage": map[string]any{
			"parentThreadId": nil, "relationshipToParent": nil, "rootThreadId": t.ID,
		},
		"pullRequests":    []any{},
		"settledAt":       nil,
		"settledOverride": nil,
		"snoozedAt":       nil,
		"snoozedUntil":    nil,
	}
}

// renderShellThread is the thread summary of the shell stream.
func renderShellThread(t *store.Thread) map[string]any {
	m := threadCommon(t)
	for k, v := range map[string]any{
		"activeRunId": nil, "activityRunStartedAt": nil, "activityRunStatus": nil,
		"autoSettleDisabledAt": nil, "blockedOnThreadWait": false, "goal": nil,
		"hasActionableProposedPlan": false, "itemCount": 0, "lastError": nil,
		"lastErrorClass": nil, "lastErrorCode": nil, "lastTitleRegeneration": nil,
		"latestRunBackgroundWake": false, "latestRunCompletedAt": nil, "latestRunId": nil,
		"latestRunRequestedAt": nil, "latestRunStartedAt": nil, "latestRunStopRequested": false,
		"latestUserAuthoredMessageAt": nil, "latestUserMessageAt": nil, "latestVisibleMessage": nil,
		"limitRecovery": nil, "opens": nil, "pendingBackgroundTasks": []any{},
		"pendingGuestMessageCount": 0, "pendingRuntimeRequest": nil, "pinOrderKey": nil,
		"pinnedAt": nil, "providerInstanceHistory": []any{}, "status": "idle",
		"titleRegeneration": nil, "unsettledAt": nil, "usageLimitResetAt": nil, "visibleItemCount": 0,
	} {
		m[k] = v
	}
	summarizeRuns(t, m)
	return m
}

// summarizeRuns fills the shell fields derived from the thread's runs,
// messages and items (spec/behavior/runs.md RUN-6).
func summarizeRuns(t *store.Thread, m map[string]any) {
	p := t.Proj
	if p == nil {
		return
	}
	for _, req := range p.List("runtimeRequests") {
		if req.String("status") == "pending" {
			m["pendingRuntimeRequest"] = map[string]any{
				"id": req.ID, "kind": req.Field("kind"), "createdAt": req.Field("createdAt"), "blocking": true,
			}
			break
		}
	}
	items := len(p.List("turnItems"))
	m["itemCount"], m["visibleItemCount"] = items, items
	history := []any{}
	seen := map[string]bool{}
	runs := p.List("runs")
	for _, r := range runs {
		if id := r.String("providerInstanceId"); id != "" && !seen[id] {
			seen[id] = true
			history = append(history, id)
		}
	}
	m["providerInstanceHistory"] = history
	latestUser := ""
	for _, msg := range p.List("messages") {
		if msg.String("role") == "user" && msg.String("createdAt") > latestUser {
			latestUser = msg.String("createdAt")
		}
	}
	if latestUser != "" {
		m["latestUserMessageAt"], m["latestUserAuthoredMessageAt"] = latestUser, latestUser
	}
	if len(runs) == 0 {
		return
	}
	r := runs[len(runs)-1]
	status := r.String("status")
	m["latestRunId"] = r.ID
	stopID := "turn-item:run:" + encodeURIComponent(r.ID) + ":signal:interrupt-request"
	m["latestRunStopRequested"] = p.Get("turnItems", stopID) != nil
	m["latestRunRequestedAt"] = r.Field("requestedAt")
	m["latestRunStartedAt"] = r.Field("startedAt")
	m["latestRunCompletedAt"] = r.Field("completedAt")
	m["status"] = status
	// The activity run is the latest run that has started; a queued run
	// behind it does not count until it starts.
	for i := len(runs) - 1; i >= 0; i-- {
		a := runs[i]
		st := a.String("status")
		if st == "queued" {
			continue
		}
		if activeRunStatuses[st] {
			m["activityRunStatus"] = st
			started := a.Field("startedAt")
			if started == nil {
				started = a.Field("requestedAt")
			}
			m["activityRunStartedAt"] = started
			if st != "waiting" {
				m["activeRunId"] = a.ID
			}
		}
		break
	}
}

// renderThreadProjection is the projection subscribeThread sends first.
func renderThreadProjection(t *store.Thread) map[string]any {
	out := map[string]any{
		"thread":    threadCommon(t),
		"updatedAt": t.UpdatedAt,
	}
	for _, name := range store.Collections {
		list := []any{}
		for _, e := range t.Proj.List(name) {
			if name == "turnItems" {
				list = append(list, wireTurnItem(e.Payload))
			} else {
				list = append(list, e.Payload)
			}
		}
		out[name] = list
	}
	visible := []any{}
	for i, e := range t.Proj.List("turnItems") {
		visible = append(visible, map[string]any{
			"item": wireTurnItem(e.Payload), "position": i, "sourceItemId": e.ID,
			"sourceThreadId": t.ID, "visibility": "local",
		})
	}
	out["visibleTurnItems"] = visible
	if t.Proj != nil && t.Proj.UpdatedAt != "" {
		out["updatedAt"] = t.Proj.UpdatedAt
	}
	return out
}

// threadStreamValue is what subscribeThread sends for an event, or nil
// when the event is not about the thread.
func threadStreamValue(e store.Event, threadID string) any {
	w, ok := e.Wire.(threadWire)
	if !ok || w.threadID != threadID {
		return nil
	}
	return map[string]any{"kind": "event", "sequence": e.Sequence, "event": w.event}
}

type threadWire struct {
	threadID string
	event    any
}

// renderThreadEvent is the thread stream's view of an event, taken right
// after it applied so payloads show the thread as of that event.
func renderThreadEvent(st *store.State, e store.Event) any {
	if store.IsV2(e.Type) {
		var env store.Envelope
		if json.Unmarshal(e.Data, &env) != nil {
			return nil
		}
		if env.Type == "turn-item.updated" {
			env.Payload = wireTurnItem(env.Payload)
		}
		return threadWire{env.ThreadID, env}
	}
	var ref struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(e.Data, &ref)
	threadID := ref.ThreadID
	t := st.Threads[threadID]
	if t == nil {
		return nil
	}
	types := map[string]string{
		store.ThreadMetadataUpdated: "thread.metadata-updated",
		store.ThreadVisited:         "thread.visited",
		store.ThreadArchived:        "thread.archived",
		store.ThreadUnarchived:      "thread.unarchived",
	}
	typ, ok := types[e.Type]
	if !ok {
		return nil
	}
	u, _ := newUUID()
	id := "event:thread:" + threadID + ":"
	if e.CommandID != "" {
		id += "command:" + e.CommandID + ":"
	}
	return threadWire{threadID, map[string]any{
		"id": id + u, "threadId": threadID, "providerInstanceId": providerInstanceID(t),
		"occurredAt": e.OccurredAt, "type": typ, "payload": threadCommon(t),
	}}
}

func shellSnapshot(st *store.State, seq int64, exclude string) map[string]any {
	projects := []any{}
	for _, id := range st.ProjectOrder {
		p := st.Projects[id]
		if p.DeletedAt == nil {
			projects = append(projects, renderShellProject(p))
		}
	}
	threads, archived := []any{}, []any{}
	for _, id := range st.ThreadOrder {
		if id == exclude {
			continue
		}
		t := st.Threads[id]
		if t.ArchivedAt != nil {
			archived = append(archived, renderShellThread(t))
		} else {
			threads = append(threads, renderShellThread(t))
		}
	}
	return map[string]any{
		"schemaVersion":    2,
		"snapshotSequence": seq,
		"projects":         projects,
		"threads":          threads,
		"archivedThreads":  archived,
	}
}
