package spec

import (
	"encoding/json"
	"testing"
)

func load(t *testing.T) *Spec {
	t.Helper()
	s, err := Load("../../../spec/generated/rpc.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecCompilesEveryMethod(t *testing.T) {
	s := load(t)
	if len(s.Methods) < 200 {
		t.Fatalf("only %d methods", len(s.Methods))
	}
	for _, m := range s.MethodNames() {
		for _, r := range []Role{Payload, Success, Error} {
			if _, err := s.schema(m, r); err != nil {
				t.Errorf("%s %s: %v", m, r, err)
			}
		}
	}
}

func TestValidateDispatchCommand(t *testing.T) {
	s := load(t)
	ok := json.RawMessage(`{"type":"thread.archive","commandId":"c1","threadId":"t1"}`)
	if err := s.Validate("orchestration.dispatchCommand", Payload, ok); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}
	bad := json.RawMessage(`{"type":"thread.archive","commandId":"c1"}`)
	if err := s.Validate("orchestration.dispatchCommand", Payload, bad); err == nil {
		t.Fatal("command without threadId accepted")
	}
}
