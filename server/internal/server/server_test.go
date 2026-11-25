package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/wire"
)

func start(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := New(Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.sessions.Issue("test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, token
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?orchestrationProtocol=2"
}

func TestRejectsBadTokenAndProtocol(t *testing.T) {
	ts, _ := start(t)
	ctx := context.Background()
	if _, err := wire.Dial(ctx, wsURL(ts), "nope", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := wire.Dial(ctx, strings.TrimSuffix(wsURL(ts), "?orchestrationProtocol=2"), "x", nil); err == nil || !strings.Contains(err.Error(), "426") {
		t.Fatalf("missing protocol: %v", err)
	}
}

func TestUnknownMethodDies(t *testing.T) {
	ts, token := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := wire.Dial(ctx, wsURL(ts), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exit, err := c.Call(ctx, "nope.nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	if exit.Tag != "Failure" || exit.Cause[0].Tag != "Die" || !strings.Contains(string(exit.Cause[0].Defect), "Unknown request tag: nope.nope") {
		t.Fatalf("unexpected exit %+v", exit)
	}
}

func TestStreamChunksAckAndInterrupt(t *testing.T) {
	ts, token := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pongs := make(chan struct{}, 1)
	c, err := wire.Dial(ctx, wsURL(ts), token, func(f wire.Frame) {
		if f.Tag == wire.TagPong {
			pongs <- struct{}{}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send(wire.Frame{Tag: wire.TagPing}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pongs:
	case <-ctx.Done():
		t.Fatal("no pong")
	}
	id, frames, err := c.Start("orchestration.subscribeShell", json.RawMessage(`{"requestCompletionMarker":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for len(kinds) < 2 {
		f := <-frames
		if f.Tag != wire.TagChunk {
			t.Fatalf("expected chunks, got %s", f.Tag)
		}
		for _, v := range f.Values {
			var item struct{ Kind string }
			_ = json.Unmarshal(v, &item)
			kinds = append(kinds, item.Kind)
		}
	}
	if strings.Join(kinds, ",") != "snapshot,synchronized" {
		t.Fatalf("kinds = %v", kinds)
	}
	if err := c.Interrupt(id); err != nil {
		t.Fatal(err)
	}
	f := <-frames
	if f.Tag != wire.TagExit || f.Exit.Tag != "Failure" || f.Exit.Cause[0].Tag != "Interrupt" {
		t.Fatalf("expected an interrupt exit, got %+v", f)
	}
}

func TestEncodeURIComponent(t *testing.T) {
	got := encodeURIComponent("pending:run:thread:a b/c~d")
	if want := "pending%3Arun%3Athread%3Aa%20b%2Fc~d"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestCheckpointRef(t *testing.T) {
	// Recorded from upstream v0.1.11 (spec/behavior/runs.md).
	got := checkpointRef("checkpoint-scope:thread:9c2e8d21-6df0-48d2-8594-cdc6889279e8:name:root", 1)
	if want := "refs/t3/orchestration-v2/checkpoints/ZWNiMTc3YThkNzNiZWZmZGMxZTYzM2Q5ZDAxMjQ4N2Q/ordinal/1"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestWireTurnItem(t *testing.T) {
	got := string(wireTurnItem(json.RawMessage(`{"type":"command_execution","output":"hi\n","exitCode":1}`)))
	if got != `{"exitCode":1,"outputIndicatesFailure":true,"outputOmitted":true,"type":"command_execution"}` {
		t.Errorf("command: %s", got)
	}
	got = string(wireTurnItem(json.RawMessage(`{"type":"file_change","diffStr":"+a","status":"completed"}`)))
	if got != `{"status":"completed","type":"file_change"}` {
		t.Errorf("file change: %s", got)
	}
	if !outputIndicatesFailure("bash: foo: command not found") || outputIndicatesFailure("all good") {
		t.Error("outputIndicatesFailure")
	}
}
