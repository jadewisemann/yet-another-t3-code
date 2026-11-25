package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sessions stores bearer tokens as SHA-256 hashes in userdata/sessions.json.
//
// This is the phase 1 stand-in for upstream's auth (pairing links, OAuth
// token exchange, DPoP, scopes); see spec/behavior/auth.md.
type Sessions struct {
	path string
	mu   sync.Mutex
}

type sessionRecord struct {
	Hash      string    `json:"hash"`
	Label     string    `json:"label"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func NewSessions(userdata string) *Sessions {
	return &Sessions{path: filepath.Join(userdata, "sessions.json")}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Sessions) load() ([]sessionRecord, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []sessionRecord
	return out, json.Unmarshal(data, &out)
}

// Issue creates a token valid for ttl.
func (s *Sessions) Issue(label string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	records = append(records, sessionRecord{Hash: hashToken(token), Label: label, IssuedAt: now, ExpiresAt: now.Add(ttl)})
	data, _ := json.MarshalIndent(records, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return "", err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return token, os.Rename(tmp, s.path)
}

// Valid reports whether token is a live session.
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return false
	}
	h := hashToken(token)
	now := time.Now()
	for _, r := range records {
		if r.Hash == h && now.Before(r.ExpiresAt) {
			return true
		}
	}
	return false
}
