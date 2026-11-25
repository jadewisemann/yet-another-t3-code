package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultScopes is what a pairing grants when no --scope is given, in
// upstream's order (scenario pairing).
var DefaultScopes = []string{
	"orchestration:read", "orchestration:operate", "settings:write", "providers:manage",
	"environment:maintain", "preview:operate", "diagnostics:read", "terminal:read",
	"terminal:operate", "source-control:write", "filesystem:read", "filesystem:write",
	"relay:read", "access:read", "access:write", "relay:write",
}

// SessionTTL is how long a token from /oauth/token lasts (expires_in 2592000).
const SessionTTL = 30 * 24 * time.Hour

// Pairing is one one-time credential, stored hashed in userdata/pairings.json.
type Pairing struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Label     string    `json:"label"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expiresAt"`
	Consumed  bool      `json:"consumed"`
}

func (s *Sessions) pairingsPath() string {
	return filepath.Join(filepath.Dir(s.path), "pairings.json")
}

func (s *Sessions) loadPairings() ([]Pairing, error) {
	data, err := os.ReadFile(s.pairingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Pairing
	return out, json.Unmarshal(data, &out)
}

func (s *Sessions) savePairings(list []Pairing) error {
	data, _ := json.MarshalIndent(list, "", "  ")
	tmp := s.pairingsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.pairingsPath())
}

// credentialAlphabet avoids characters that are easy to misread.
const credentialAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// CreatePairing makes a one-time credential (12 characters, like upstream's).
func (s *Sessions) CreatePairing(label string, scopes []string, ttl time.Duration) (Pairing, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadPairings()
	if err != nil {
		return Pairing{}, "", err
	}
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	var b strings.Builder
	for _, c := range raw {
		b.WriteByte(credentialAlphabet[int(c)%len(credentialAlphabet)])
	}
	credential := b.String()
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	id, _ := newUUID()
	p := Pairing{ID: id, Hash: hashToken(credential), Label: label, Scopes: scopes, ExpiresAt: time.Now().UTC().Add(ttl)}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return Pairing{}, "", err
	}
	return p, credential, s.savePairings(append(list, p))
}

// ConsumePairing marks a live credential used and returns it.
func (s *Sessions) ConsumePairing(credential string) (Pairing, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadPairings()
	if err != nil {
		return Pairing{}, false
	}
	h := hashToken(credential)
	for i, p := range list {
		if p.Hash == h && !p.Consumed && time.Now().Before(p.ExpiresAt) {
			list[i].Consumed = true
			if s.savePairings(list) != nil {
				return Pairing{}, false
			}
			return p, true
		}
	}
	return Pairing{}, false
}

const (
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	bootstrapTokenType = "urn:t3:params:oauth:token-type:environment-bootstrap"
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token"
)

// oauthToken exchanges a pairing credential for a bearer token
// (spec/behavior/auth.md AUTH-4, scenario pairing).
func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		authInvalid(w, "invalid_request")
		return
	}
	if r.PostForm.Get("grant_type") != tokenExchangeGrant || r.PostForm.Get("subject_token_type") != bootstrapTokenType {
		authInvalid(w, "unsupported_grant")
		return
	}
	p, ok := s.sessions.ConsumePairing(r.PostForm.Get("subject_token"))
	if !ok {
		authInvalid(w, "invalid_credential")
		return
	}
	label := p.Label
	if label == "" {
		label = "paired client"
	}
	token, err := s.sessions.Issue(label, SessionTTL)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{
		"access_token":      token,
		"expires_in":        int(SessionTTL.Seconds()),
		"issued_token_type": accessTokenType,
		"scope":             strings.Join(p.Scopes, " "),
		"token_type":        "Bearer",
	})
}

func authInvalid(w http.ResponseWriter, reason string) {
	trace := make([]byte, 16)
	_, _ = rand.Read(trace)
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"_tag": "EnvironmentAuthInvalidError", "code": "auth_invalid",
		"reason": reason, "traceId": hex.EncodeToString(trace),
	})
}
