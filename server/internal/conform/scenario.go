// Package conform runs language-neutral conformance scenarios against a T3
// Code server (the upstream one or ours) and compares normalized transcripts
// with golden files recorded from upstream.
package conform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scenario is one file in conformance/scenarios.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Tier says which porting phase must pass this scenario (see docs/go-port/PLAN.md).
	Tier  string `json:"tier"`
	Steps []Step `json:"steps"`
}

// Step is one action. Exactly one of the op-specific fields is used.
//
//	call       send a request and wait for its Exit
//	subscribe  send a stream request; frames are collected in the background
//	await      wait until stream `stream` has sent a chunk value matching `match`
//	interrupt  end stream `stream` and wait for its Exit
//	http       send a plain HTTP request (no auth unless `auth` is true)
type Step struct {
	Op      string          `json:"op"`
	As      string          `json:"as,omitempty"`     // name for a call or stream
	Method  string          `json:"method,omitempty"` // RPC method
	Payload json.RawMessage `json:"payload,omitempty"`
	Stream  string          `json:"stream,omitempty"`
	// Match is a JSON object; a value matches when every key in Match is
	// present with an equal value (recursively).
	Match json.RawMessage `json:"match,omitempty"`
	// TimeoutMs bounds call/await/interrupt. Default 15000.
	TimeoutMs int `json:"timeoutMs,omitempty"`

	// http
	HTTPMethod string `json:"httpMethod,omitempty"`
	Path       string `json:"path,omitempty"`
	Auth       bool   `json:"auth,omitempty"`
	// Record controls which part of a response goes into the transcript:
	// "full" (default), "status", or "keys" (top-level keys only).
	Record string `json:"record,omitempty"`
}

// LoadScenarios reads every *.json file in dir, sorted by name.
func LoadScenarios(dir string, only []string) ([]Scenario, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Scenario
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if s.Name == "" {
			s.Name = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		if len(only) > 0 && !contains(only, s.Name) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
