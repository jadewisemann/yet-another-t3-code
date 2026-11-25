package conform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Golden is the file stored in conformance/golden/<scenario>.json.
type Golden struct {
	Scenario   string  `json:"scenario"`
	RecordedOn string  `json:"recordedOn"` // server version of the upstream recording
	Transcript []Entry `json:"transcript"`
}

func Marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}

// Diff compares two transcripts by their pretty JSON and describes the first
// differences, or returns "" when they are equal.
func Diff(want, got []Entry) string {
	a := strings.Split(string(Marshal(want)), "\n")
	b := strings.Split(string(Marshal(got)), "\n")
	if strings.Join(a, "\n") == strings.Join(b, "\n") {
		return ""
	}
	// A small LCS diff is plenty for transcripts of a few hundred lines.
	n, m := len(a), len(b)
	if n*m > 4_000_000 {
		return fmt.Sprintf("transcripts differ (%d vs %d lines)", n, m)
	}
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	// Each change is shown with the golden line number and the closest
	// earlier "type"/"kind"/"step" line, which says where in the transcript
	// it is.
	var out strings.Builder
	shown := 0
	i, j := 0, 0
	lastWhere := -1
	where := func(i int) {
		for k := i; k >= 0 && k < n; k-- {
			t := strings.TrimSpace(a[k])
			if strings.HasPrefix(t, `"type":`) || strings.HasPrefix(t, `"kind":`) || strings.HasPrefix(t, `"step":`) {
				if k != lastWhere {
					lastWhere = k
					fmt.Fprintf(&out, "@ line %d, under %s\n", i+1, t)
				}
				return
			}
		}
	}
	for (i < n || j < m) && shown < 60 {
		switch {
		case i < n && j < m && a[i] == b[j]:
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			where(i)
			fmt.Fprintf(&out, "+ %s\n", b[j])
			j++
			shown++
		default:
			where(i)
			fmt.Fprintf(&out, "- %s\n", a[i])
			i++
			shown++
		}
	}
	return out.String()
}
