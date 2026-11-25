package conform

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Vars expands `${...}` placeholders in payloads and remembers every
// generated value so the normalizer can put the placeholder name back.
//
//	${workspace}   the server's working directory
//	${uuid:name}   a fresh UUID, the same one for every use of `name`
//	${id:name}     like uuid, but a short opaque id ("name-<hex>")
//	${now}         the current time as an ISO 8601 string
//	${var:name}    a value an earlier step captured
type Vars struct {
	fixed     map[string]string
	generated map[string]string
	captured  map[string]string
}

// Set stores a captured value for `${var:name}`.
func (v *Vars) Set(name, value string) { v.captured[name] = value }

func NewVars(fixed map[string]string) *Vars {
	return &Vars{fixed: fixed, generated: map[string]string{}, captured: map[string]string{}}
}

var placeholder = regexp.MustCompile(`\$\{([a-z]+)(?::([A-Za-z0-9_.-]+))?\}`)

func (v *Vars) Expand(raw json.RawMessage) (json.RawMessage, error) {
	var err error
	out := placeholder.ReplaceAllStringFunc(string(raw), func(m string) string {
		parts := placeholder.FindStringSubmatch(m)
		kind, name := parts[1], parts[2]
		switch kind {
		case "var":
			val, ok := v.captured[name]
			if !ok {
				err = fmt.Errorf("nothing captured as %q yet", name)
			}
			enc, _ := json.Marshal(val)
			return strings.Trim(string(enc), `"`)
		case "now":
			return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		case "uuid", "id":
			key := kind + ":" + name
			if val, ok := v.generated[key]; ok {
				return val
			}
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			var val string
			if kind == "uuid" {
				b[6] = (b[6] & 0x0f) | 0x40
				b[8] = (b[8] & 0x3f) | 0x80
				val = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
			} else {
				val = fmt.Sprintf("%s-%x", name, b[:6])
			}
			v.generated[key] = val
			return val
		default:
			if val, ok := v.fixed[kind]; ok {
				// Values land inside JSON strings.
				enc, _ := json.Marshal(val)
				return strings.Trim(string(enc), `"`)
			}
			err = fmt.Errorf("unknown placeholder %s", m)
			return m
		}
	})
	return json.RawMessage(out), err
}

// Replacements returns value → placeholder pairs, longest value first.
func (v *Vars) Replacements() [][2]string {
	var out [][2]string
	for k, val := range v.fixed {
		out = append(out, [2]string{val, "<" + k + ">"})
	}
	for k, val := range v.generated {
		out = append(out, [2]string{val, "<" + k + ">"})
	}
	for k, val := range v.captured {
		out = append(out, [2]string{val, "<var:" + k + ">"})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i][0]) > len(out[j][0]) })
	return out
}
