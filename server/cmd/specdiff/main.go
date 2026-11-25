// Command specdiff compares two spec/generated/rpc.json files and lists the
// methods that were added, removed, or changed. A method changes when the
// fully resolved schema of its payload, success value, or error changes.
//
//	specdiff old/rpc.json new/rpc.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type doc struct {
	Upstream map[string]any            `json:"upstream"`
	Methods  map[string]map[string]any `json:"methods"`
	Defs     map[string]any            `json:"$defs"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: specdiff old.json new.json")
		os.Exit(2)
	}
	a, b := load(os.Args[1]), load(os.Args[2])
	fmt.Printf("old: %v %v\nnew: %v %v\n\n", a.Upstream["tag"], short(a.Upstream["commit"]), b.Upstream["tag"], short(b.Upstream["commit"]))
	var added, removed, changed []string
	for m := range b.Methods {
		if _, ok := a.Methods[m]; !ok {
			added = append(added, m)
		}
	}
	for m := range a.Methods {
		if _, ok := b.Methods[m]; !ok {
			removed = append(removed, m)
			continue
		}
		var parts []string
		for _, role := range []string{"kind", "payload", "success", "error"} {
			if digest(a, a.Methods[m][role]) != digest(b, b.Methods[m][role]) {
				parts = append(parts, role)
			}
		}
		if len(parts) > 0 {
			changed = append(changed, m+" ("+strings.Join(parts, ", ")+")")
		}
	}
	for _, l := range [][]string{added, removed, changed} {
		sort.Strings(l)
	}
	section("added", added)
	section("removed", removed)
	section("changed", changed)
	if len(added)+len(removed)+len(changed) > 0 {
		os.Exit(1)
	}
}

func section(name string, items []string) {
	fmt.Printf("%s: %d\n", name, len(items))
	for _, i := range items {
		fmt.Println("  " + i)
	}
}

func short(v any) string {
	s, _ := v.(string)
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func load(path string) *doc {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var d doc
	if err := json.Unmarshal(data, &d); err != nil {
		fmt.Fprintln(os.Stderr, path, err)
		os.Exit(2)
	}
	return &d
}

// digest hashes a schema with every $ref replaced by the digest of its
// target, so a change anywhere below a method changes the method's digest.
func digest(d *doc, v any) string {
	memo := map[string]string{}
	var walk func(v any, stack map[string]bool) any
	walk = func(v any, stack map[string]bool) any {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
				name := strings.TrimPrefix(ref, "#/$defs/")
				if h, ok := memo[name]; ok {
					return h
				}
				if stack[name] {
					return "recursive:" + name
				}
				stack[name] = true
				h := hash(walk(d.Defs[name], stack))
				delete(stack, name)
				memo[name] = h
				return h
			}
			out := map[string]any{}
			for k, e := range x {
				out[k] = walk(e, stack)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e, stack)
			}
			return out
		default:
			return v
		}
	}
	return hash(walk(v, map[string]bool{}))
}

func hash(v any) string {
	data, _ := json.Marshal(v) // map keys are sorted by encoding/json
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
