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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
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

// checkpointFiles summarizes what changed between two checkpoint refs, the
// way upstream CheckpointService does: a numstat diff, sorted by path, with
// paths the turn did not write (git history that arrived while HEAD moved)
// left out. It is empty when the previous checkpoint is missing.
func checkpointFiles(cwd, fromRef, toRef string) []any {
	files := []any{}
	if _, err := git(cwd, nil, "rev-parse", "--verify", "-q", fromRef+"^{commit}"); err != nil {
		return files
	}
	out, err := gitRaw(cwd, "-c", "diff.relative=false", "-c", "diff.suppressBlankEmpty=false",
		"diff", "--numstat", "-z", "--no-color", "--no-ext-diff", "--no-textconv", "--submodule=short",
		fromRef+"^{commit}", toRef+"^{commit}")
	if err != nil {
		return files
	}
	authored := authoredPaths(cwd, fromRef, toRef)
	for _, f := range parseNumstat(out) {
		if authored != nil && !authored[f.path] && (f.previous == "" || !authored[f.previous]) {
			continue
		}
		files = append(files, map[string]any{
			"path": f.path, "kind": "modified", "additions": f.additions, "deletions": f.deletions,
		})
	}
	return files
}

type numstatFile struct {
	path, previous       string
	additions, deletions int
}

var numstatCounts = regexp.MustCompile(`^(\d+|-)\t(\d+|-)\t`)

// parseNumstat reads `git diff --numstat -z` (upstream
// parseTurnDiffFilesFromNumstat). A rename has an empty path followed by
// the old and new paths as separate records.
func parseNumstat(out string) []numstatFile {
	records := strings.Split(out, "\x00")
	var files []numstatFile
	for i := 0; i < len(records); i++ {
		c := numstatCounts.FindStringSubmatch(records[i])
		if c == nil {
			continue
		}
		f := numstatFile{path: records[i][len(c[0]):]}
		if f.path == "" {
			if i+1 < len(records) {
				f.previous = records[i+1]
			}
			if i+2 < len(records) {
				f.path = records[i+2]
			}
			i += 2
		}
		if f.path == "" {
			continue
		}
		f.additions, _ = strconv.Atoi(c[1])
		f.deletions, _ = strconv.Atoi(c[2])
		files = append(files, f)
	}
	// Upstream sorts with localeCompare, which is the root collation.
	col := collate.New(language.Und)
	sort.SliceStable(files, func(a, b int) bool { return col.CompareString(files[a].path, files[b].path) < 0 })
	return files
}

// authoredPaths is nil when HEAD did not move between the two checkpoints,
// so every changed path belongs to the turn. When HEAD moved, it holds the
// paths the turn wrote: uncommitted changes at either end, merge conflict
// fixes, and the paths of commits made since the previous checkpoint.
// Upstream also drops commits that a fetch delivered; this keeps them.
func authoredPaths(cwd, fromRef, toRef string) map[string]bool {
	heads, err := git(cwd, nil, "log", "--no-walk=unsorted", "--format=%ct %P", fromRef+"^{commit}", toRef+"^{commit}")
	if err != nil {
		return nil
	}
	lines := strings.Split(heads, "\n")
	if len(lines) < 2 {
		return nil
	}
	from, to := strings.Fields(lines[0]), strings.Fields(lines[1])
	if len(from) < 2 || len(to) < 2 || from[1] == to[1] {
		return nil
	}
	capturedAt, startHead, endHead := from[0], from[1], to[1]
	paths := map[string]bool{}
	add := func(args ...string) {
		out, err := gitRaw(cwd, append(append([]string{"-c", "diff.relative=false", "-c", "log.showSignature=false"}, args...),
			"--name-only", "-z", "--no-renames", "--no-ext-diff")...)
		if err != nil {
			return
		}
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				paths[p] = true
			}
		}
	}
	add("diff", startHead, fromRef+"^{commit}")
	add("diff", endHead, toRef+"^{commit}")
	add("log", "--format=", "--diff-merges=remerge", endHead+".."+startHead)
	if commits, err := git(cwd, nil, "rev-list", "--since=@"+capturedAt, startHead+".."+endHead); err == nil {
		for _, c := range strings.Fields(commits) {
			add("log", "--no-walk=unsorted", "--format=", "--diff-merges=remerge", c)
		}
	}
	return paths
}

// gitRaw is git without trimming the output, for NUL-separated lists.
func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}
