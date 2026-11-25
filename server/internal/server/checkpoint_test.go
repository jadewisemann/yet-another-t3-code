package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	out := "3\t1\tb.txt\x00-\t-\timg.png\x00\x002\t0\t\x00old.txt\x00A.txt\x00"
	got := parseNumstat(out)
	want := []numstatFile{
		{path: "A.txt", previous: "old.txt", additions: 2},
		{path: "b.txt", additions: 3, deletions: 1},
		{path: "img.png"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCapCheckpointFiles(t *testing.T) {
	files := []any{}
	for i := 0; i < 205; i++ {
		files = append(files, map[string]any{"path": "f", "kind": "modified", "additions": 2, "deletions": 1})
	}
	raw, _ := json.Marshal(map[string]any{"id": "c", "files": files})
	var m map[string]any
	_ = json.Unmarshal(wireCheckpoint(raw), &m)
	if n := len(m["files"].([]any)); n != 200 {
		t.Fatalf("files = %d", n)
	}
	total := m["filesTotal"].(map[string]any)
	if total["count"].(float64) != 205 || total["additions"].(float64) != 410 || total["deletions"].(float64) != 205 {
		t.Fatalf("filesTotal = %v", total)
	}
}

func TestCheckpointFilesWhenHeadMoves(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := git(dir, checkpointEnv, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("a.txt", "a\n")
	run("add", "a.txt")
	run("commit", "-q", "-m", "a")
	from, to := checkpointRef("scope", 0), checkpointRef("scope", 1)
	if ok, err := captureCheckpoint(dir, from); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The turn commits one file and leaves another uncommitted.
	write("b.txt", "b\n")
	run("add", "b.txt")
	run("commit", "-q", "-m", "b")
	write("c.txt", "c\nc\n")
	if ok, err := captureCheckpoint(dir, to); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, _ := json.Marshal(checkpointFiles(dir, from, to))
	want := `[{"additions":1,"deletions":0,"kind":"modified","path":"b.txt"},{"additions":2,"deletions":0,"kind":"modified","path":"c.txt"}]`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
}
