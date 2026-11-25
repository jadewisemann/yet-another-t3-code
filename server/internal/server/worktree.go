package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Worktree threads (spec/behavior/worktrees.md): a thread launched with the
// worktree strategy runs in a git worktree of its own, created on a new
// branch under <base-dir>/worktrees/<repository>/ before its first run
// starts. existing_worktree runs in a worktree the client names.

type workspaceStrategy struct {
	Type         string  `json:"type"`
	Branch       *string `json:"branch,omitempty"`
	BaseRef      string  `json:"baseRef,omitempty"`
	WorktreePath string  `json:"worktreePath,omitempty"`
}

// temporaryWorktreeBranch is upstream's name for a worktree branch the
// client did not name: t3/<8 hex digits>.
func temporaryWorktreeBranch() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "t3/" + hex.EncodeToString(b), nil
}

// worktreeFolder is where upstream puts a managed worktree: the branch with
// slashes as dashes, cut to 23 bytes, then 16 hex digits of
// sha256(<git common dir>\0<branch>); a taken folder gets -2, -3, ...
func (s *Server) worktreeFolder(repo, branch string) (string, error) {
	common, err := git(repo, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	physical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		physical = repo
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(physical, common)
	}
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real
	}
	sum := sha256.Sum256([]byte(common + "\x00" + branch))
	prefix := ""
	for _, r := range strings.ReplaceAll(branch, "/", "-") {
		if len(prefix)+len(string(r)) > 23 {
			break
		}
		prefix += string(r)
	}
	abs, _ := filepath.Abs(repo)
	base := filepath.Join(s.cfg.BaseDir, "worktrees", filepath.Base(abs), prefix+"-"+hex.EncodeToString(sum[:])[:16])
	path := base
	for n := 2; ; n++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		path = fmt.Sprintf("%s-%d", base, n)
	}
}

// createWorktree makes branch at baseRef in repo and checks it out in a new
// managed worktree, returning the worktree's path.
func (s *Server) createWorktree(repo, branch, baseRef string) (string, error) {
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(baseRef, "-") {
		return "", fmt.Errorf("refusing a ref that looks like an option")
	}
	start := baseRef
	if _, err := git(repo, nil, "rev-parse", "--verify", "-q", start+"^{commit}"); err != nil {
		// Upstream falls back to HEAD when the base does not exist.
		if _, err := git(repo, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
			return "", fmt.Errorf("base ref %q not found and the repository has no commits", baseRef)
		}
		start = "HEAD"
	}
	path, err := s.worktreeFolder(repo, branch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := git(repo, nil, "branch", "--", branch, start); err != nil {
		return "", err
	}
	if _, err := git(repo, nil, "worktree", "add", "--", path, branch); err != nil {
		_, _ = git(repo, nil, "branch", "-D", "--", branch)
		return "", err
	}
	return path, nil
}

// prepareWorktree creates the thread's worktree for a deferred run and
// records it on the thread, so the run starts there.
func (s *Server) prepareWorktree(ctx context.Context, r *runState, launchCommandID string, strategy workspaceStrategy) error {
	var repo string
	var branch *string
	s.store.Read(func(st *store.State, _ int64) {
		if t := st.Threads[r.ids.thread]; t != nil {
			branch = t.Branch
			if p := st.Projects[t.ProjectID]; p != nil {
				repo = p.WorkspaceRoot
			}
		}
	})
	if repo == "" || branch == nil {
		return errors.New("the thread's project or branch is missing")
	}
	path, err := s.createWorktree(repo, *branch, strategy.BaseRef)
	if err != nil {
		return err
	}
	r.cwd = path
	_, err = s.store.Decide(ctx, launchCommandID+":workspace", func(st *store.State) (string, any, error) {
		return store.ThreadMetadataUpdated, store.ThreadMetadata{
			ThreadID: r.ids.thread, Branch: branch, WorktreePath: &path, At: store.Now(),
		}, nil
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return err
	}
	return nil
}

func rawStrategy(s workspaceStrategy) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
