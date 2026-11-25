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
//	cli        run the server binary with `args` (for example `auth pairing create`)
//	file       read a file under the data directory while the server runs
//	connect    open another /ws socket with `bearer` and record the status
type Step struct {
	Op      string          `json:"op"`
	As      string          `json:"as,omitempty"`     // name for a call or stream
	Method  string          `json:"method,omitempty"` // RPC method
	Payload json.RawMessage `json:"payload,omitempty"`
	Stream  string          `json:"stream,omitempty"`
	// Match is a JSON object; a value matches when every key in Match is
	// present with an equal value (recursively).
	Match json.RawMessage `json:"match,omitempty"`
	// Ignore lists field names whose values this step does not record
	// (they become "<ignored>"), for values upstream itself does not pin down.
	Ignore []string `json:"ignore,omitempty"`
	// TimeoutMs bounds call/await/interrupt. Default 15000.
	TimeoutMs int `json:"timeoutMs,omitempty"`

	// http
	HTTPMethod string `json:"httpMethod,omitempty"`
	Path       string `json:"path,omitempty"`
	Auth       bool   `json:"auth,omitempty"`
	// Bearer sends this token instead (expanded), for example "${var:access}".
	Bearer string `json:"bearer,omitempty"`
	// Body and ContentType are sent with an http step (Body is expanded).
	Body        string `json:"body,omitempty"`
	ContentType string `json:"contentType,omitempty"`

	// cli: arguments after the binary, expanded. Every cli call gets
	// `--base-dir <base>` appended.
	Args []string `json:"args,omitempty"`
	// Capture stores top-level string fields of a JSON result (cli stdout,
	// http body, or call success value) as `${var:<name>}`: {"name": "field"}.
	Capture map[string]string `json:"capture,omitempty"`

	// file: path relative to the data directory.
	File string `json:"file,omitempty"`

	// Record controls which part of a result goes into the transcript:
	// "full" (default), "status" (http status / cli exit code only), or
	// "keys" (sorted top-level keys of the JSON result). On an await step,
	// "match" keeps only the matching value, for streams whose earlier
	// values depend on timing.
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
