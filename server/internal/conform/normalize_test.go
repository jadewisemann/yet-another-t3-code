package conform

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizer(t *testing.T) {
	v := NewVars(map[string]string{"workspace": "/tmp/w"})
	payload, err := v.Expand(json.RawMessage(`{"a":"${uuid:p}","b":"${uuid:p}","c":"${workspace}/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	_ = json.Unmarshal(payload, &in)
	if in["a"] != in["b"] || len(in["a"]) != 36 {
		t.Fatalf("uuid not reused: %v", in)
	}
	n := NewNormalizer(v)
	raw, _ := json.Marshal(map[string]any{
		"id": in["a"], "path": "/tmp/w/x", "at": "2026-11-25T05:42:04.988Z",
		"other": "8f9c2a40-1111-4222-8333-944455556666", "again": "8f9c2a40-1111-4222-8333-944455556666",
		"fiberId": 1213, "ms": 1764049324000, "n": 3,
	})
	var buf bytes.Buffer
	_ = json.Compact(&buf, Marshal(n.Value(raw)))
	got := buf.String()
	want := `{"again":"<uuid#1>","at":"<time>","fiberId":"<fiberId>","id":"<uuid:p>","ms":"<epoch-ms>","n":3,"other":"<uuid#1>","path":"<workspace>/x"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMatches(t *testing.T) {
	var want, got any
	_ = json.Unmarshal([]byte(`{"kind":"thread.updated","thread":{"title":"A"}}`), &want)
	_ = json.Unmarshal([]byte(`{"kind":"thread.updated","sequence":3,"thread":{"title":"A","id":"x"}}`), &got)
	if !matches(want, got) {
		t.Fatal("subset should match")
	}
	_ = json.Unmarshal([]byte(`{"kind":"thread.updated","thread":{"title":"B"}}`), &got)
	if matches(want, got) {
		t.Fatal("different title matched")
	}
}
