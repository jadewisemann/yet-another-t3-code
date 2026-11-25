package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Checkpoints snapshot the workspace into a git ref without touching the
// user's index, branches or working tree (spec/behavior/runs.md). The ref
// name is upstream's: refs/t3/orchestration-v2/checkpoints/<key>/ordinal/<n>
// with key = base64url(hex(sha256(scopeId))[:32]).

const checkpointRefsPrefix = "refs/t3/orchestration-v2/checkpoints"

func checkpointRef(scopeID string, ordinal int) string {
	sum := sha256.Sum256([]byte(scopeID))
	key := hex.EncodeToString(sum[:])[:32]
	return fmt.Sprintf("%s/%s/ordinal/%d", checkpointRefsPrefix, base64.RawURLEncoding.EncodeToString([]byte(key)), ordinal)
}

var checkpointEnv = []string{
	"GIT_AUTHOR_NAME=T3 Code", "GIT_AUTHOR_EMAIL=t3code@users.noreply.github.com",
	"GIT_COMMITTER_NAME=T3 Code", "GIT_COMMITTER_EMAIL=t3code@users.noreply.github.com",
}

func git(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// captureCheckpoint writes the workspace (tracked and untracked, not
// ignored files) as a commit on ref. It returns false when cwd is not a git
// repository.
func captureCheckpoint(cwd, ref string) (bool, error) {
	if _, err := git(cwd, nil, "rev-parse", "--is-inside-work-tree"); err != nil {
		return false, nil
	}
	tmp, err := os.MkdirTemp("", "yatt-checkpoint-")
	if err != nil {
		return true, err
	}
	defer os.RemoveAll(tmp)
	env := append([]string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}, checkpointEnv...)
	head, headErr := git(cwd, nil, "rev-parse", "--verify", "-q", "HEAD")
	if headErr == nil {
		if _, err := git(cwd, env, "read-tree", "HEAD"); err != nil {
			return true, err
		}
	}
	if _, err := git(cwd, env, "-c", "core.safecrlf=false", "add", "-A", "--", "."); err != nil {
		return true, err
	}
	tree, err := git(cwd, env, "write-tree")
	if err != nil {
		return true, err
	}
	args := []string{"commit-tree", tree}
	if headErr == nil {
		args = append(args, "-p", head)
	}
	args = append(args, "-m", "t3 checkpoint ref="+ref)
	commit, err := git(cwd, env, args...)
	if err != nil {
		return true, err
	}
	_, err = git(cwd, nil, "update-ref", ref, commit)
	return true, err
}
