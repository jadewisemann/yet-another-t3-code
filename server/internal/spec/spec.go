// Package spec loads spec/generated/rpc.json, the machine-readable contract
// extracted from upstream T3 Code, and validates wire values against it.
package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const docURL = "https://yatt.invalid/spec/rpc.json"

// Method describes one RPC method.
type Method struct {
	Kind        string   `json:"kind"` // "unary" or "stream"
	Middlewares []string `json:"middlewares"`
}

// Upstream records where the spec was generated from.
type Upstream struct {
	Repository     string `json:"repository"`
	Commit         string `json:"commit"`
	CommittedAt    string `json:"committedAt"`
	Tag            string `json:"tag"`
	PackageVersion string `json:"packageVersion"`
}

// Spec is a loaded rpc.json.
type Spec struct {
	Upstream Upstream
	Methods  map[string]Method

	compiler *jsonschema.Compiler
	mu       sync.Mutex
	cache    map[string]*jsonschema.Schema
}

// Load reads and indexes rpc.json.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var head struct {
		Upstream Upstream          `json:"upstream"`
		Methods  map[string]Method `json:"methods"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(docURL, doc); err != nil {
		return nil, err
	}
	return &Spec{Upstream: head.Upstream, Methods: head.Methods, compiler: c, cache: map[string]*jsonschema.Schema{}}, nil
}

// MethodNames returns every method, sorted.
func (s *Spec) MethodNames() []string {
	out := make([]string, 0, len(s.Methods))
	for k := range s.Methods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Role is the part of a method a value belongs to.
type Role string

const (
	Payload Role = "payload"
	Success Role = "success" // the Exit value of a unary call, or one chunk value of a stream
	Error   Role = "error"
)

// Validate checks one JSON value against a method's schema for role.
func (s *Spec) Validate(method string, role Role, value json.RawMessage) error {
	if _, ok := s.Methods[method]; !ok {
		return fmt.Errorf("method %q is not in the spec", method)
	}
	sch, err := s.schema(method, role)
	if err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(value))
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

func (s *Spec) schema(method string, role Role) (*jsonschema.Schema, error) {
	key := method + "/" + string(role)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.cache[key]; ok {
		return sch, nil
	}
	sch, err := s.compiler.Compile(docURL + "#/methods/" + method + "/" + string(role))
	if err != nil {
		return nil, err
	}
	s.cache[key] = sch
	return sch, nil
}
