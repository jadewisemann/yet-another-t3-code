package conform

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// Checkpoint refs embed base64url(hex(sha256(scopeId))[:32]); the scope id
	// contains the thread id, which is fresh per run.
	checkpointRefRe = regexp.MustCompile(`(refs/t3/orchestration-v2/checkpoints/)[A-Za-z0-9_-]+(/ordinal/)`)
	// Managed worktree folders end in a hash of the repository's git
	// directory and the branch, and the repository is fresh per run.
	worktreeDirRe = regexp.MustCompile(`(/worktrees/[^/"]+/[^/"]*-)[0-9a-f]{16}\b`)
	timeRe        = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})`)
)

// volatileFields are fields whose value depends on the run, not on the
// behavior under test. Their values become "<field>".
var volatileFields = map[string]bool{
	"traceId":    true, // random trace id in auth errors
	"expires_in": true, // seconds left, which ticks during the run
	"fiberId":    true, // Effect fiber ids in Interrupt causes
	"pid":        true,
	"port":       true, // a free port picked per run
}

// Normalizer replaces values that differ between runs (ids, times, paths)
// with stable placeholders. One normalizer is used for a whole transcript, so
// the same unknown UUID always becomes the same `<uuid#n>`.
type Normalizer struct {
	replacements [][2]string
	uuids        map[string]string
}

func NewNormalizer(v *Vars) *Normalizer {
	return &Normalizer{replacements: v.Replacements(), uuids: map[string]string{}}
}

func (n *Normalizer) Value(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Sprintf("<invalid json: %v>", err)
	}
	return n.walk("", v)
}

func (n *Normalizer) walk(key string, v any) any {
	if volatileFields[key] {
		if _, isObject := v.(map[string]any); !isObject {
			return "<" + key + ">"
		}
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[k] = n.walk(k, x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.walk(key, e)
		}
		return out
	case string:
		return n.str(x)
	case json.Number:
		// Millisecond timestamps.
		if i, err := x.Int64(); err == nil && i > 1_000_000_000_000 && i < 10_000_000_000_000 {
			return "<epoch-ms>"
		}
		return x
	default:
		return v
	}
}

func (n *Normalizer) str(s string) string {
	for _, r := range n.replacements {
		if r[0] != "" {
			s = strings.ReplaceAll(s, r[0], r[1])
		}
	}
	s = timeRe.ReplaceAllString(s, "<time>")
	s = checkpointRefRe.ReplaceAllString(s, "${1}<scope-key>${2}")
	s = worktreeDirRe.ReplaceAllString(s, "${1}<hash>")
	s = uuidRe.ReplaceAllStringFunc(s, func(u string) string {
		u = strings.ToLower(u)
		if p, ok := n.uuids[u]; ok {
			return p
		}
		p := fmt.Sprintf("<uuid#%d>", len(n.uuids)+1)
		n.uuids[u] = p
		return p
	})
	return s
}
