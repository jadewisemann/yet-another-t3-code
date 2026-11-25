package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// getConfig answers server.getConfig with every key upstream sends (scenario
// server-config). Values are the ones for a server with no providers,
// editors, or optional features: P2 fills in providers, and each feature flag
// turns true when its phase lands. Settings are empty, which upstream's
// decoder reads as all defaults.
func (s *Server) getConfig(ctx context.Context, payload json.RawMessage) (any, error) {
	ud := UserData(s.cfg.BaseDir)
	host, _ := os.Hostname()
	return map[string]any{
		"environment": map[string]any{
			"environmentId": s.envID,
			"label":         host,
			"platform":      map[string]string{"os": platformOS(), "arch": goArch()},
			"serverVersion": Version,
			"capabilities":  map[string]any{},
		},
		"auth": map[string]any{
			"policy":            "loopback-browser",
			"bootstrapMethods":  []string{"one-time-token"},
			"sessionMethods":    []string{"bearer-access-token"},
			"sessionCookieName": "yatt_session",
		},
		"cwd":                   s.cfg.Cwd,
		"hostname":              host,
		"keybindingsConfigPath": filepath.Join(ud, "keybindings.json"),
		"keybindings":           []any{},
		"issues":                []any{},
		"providers":             []any{},
		"availableEditors":      []any{},
		"directEndpoints":       []any{},
		"remoteOpenTargets":     []any{},
		"newProjectsRoot":       filepath.Join(s.cfg.BaseDir, "projects"),
		"scratchWorkspaceRoot":  filepath.Join(s.cfg.BaseDir, "scratch"),
		"serverTime":            store.Now(),
		"settings":              map[string]any{},
		"observability": map[string]any{
			"logsDirectoryPath":   filepath.Join(ud, "logs"),
			"localTracingEnabled": false,
			"otlpTracesEnabled":   false,
			"otlpMetricsEnabled":  false,
			"otlpLogsEnabled":     false,
		},
		"secretProviderSettings":       false,
		"secretReferences":             false,
		"secretRequestReferences":      false,
		"shellOlderSettled":            false,
		"shellResumeCompletionMarker":  false,
		"shellThreadFamilies":          false,
		"threadFind":                   false,
		"threadFindProgressive":        false,
		"threadLinkMetadata":           false,
		"threadResumeCompletionMarker": false,
		"threadSnapshotPagination":     false,
	}, nil
}

func platformOS() string {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return runtime.GOOS
	}
	return "unknown"
}
