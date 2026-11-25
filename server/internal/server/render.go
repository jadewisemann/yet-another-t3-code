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
		"activeProviderThreadId": nil,
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
	return m
}

// renderThreadProjection is the projection subscribeThread sends first.
func renderThreadProjection(t *store.Thread) map[string]any {
	empty := func() []any { return []any{} }
	return map[string]any{
		"thread":           threadCommon(t),
		"attempts":         empty(),
		"checkpointScopes": empty(),
		"checkpoints":      empty(),
		"contextHandoffs":  empty(),
		"contextTransfers": empty(),
		"messages":         empty(),
		"nodes":            empty(),
		"plans":            empty(),
		"providerSessions": empty(),
		"providerThreads":  empty(),
		"providerTurns":    empty(),
		"runs":             empty(),
		"runtimeRequests":  empty(),
		"subagents":        empty(),
		"turnItems":        empty(),
		"visibleTurnItems": empty(),
		"updatedAt":        t.UpdatedAt,
	}
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
