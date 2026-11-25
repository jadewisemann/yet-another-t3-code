package server

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// WriteRuntimeState records where the server listens in
// userdata/server-runtime.json, in upstream's format (scenario
// runtime-state), so clients on this machine can find it. Call the returned
// function on shutdown to remove the file.
func (s *Server) WriteRuntimeState(addr net.Addr) (func(), error) {
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(portStr)
	ownerID, err := loadOrCreateID(filepath.Join(UserData(s.cfg.BaseDir), "owner-id"))
	if err != nil {
		return nil, err
	}
	state := map[string]any{
		"version":       1,
		"host":          host,
		"port":          port,
		"origin":        fmt.Sprintf("http://%s", net.JoinHostPort(host, portStr)),
		"pid":           os.Getpid(),
		"ownerId":       ownerID,
		"serverVersion": Version,
		"startedAt":     store.Now(),
		"managedPolicy": nil,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	path := filepath.Join(UserData(s.cfg.BaseDir), "server-runtime.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}
