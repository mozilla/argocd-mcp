package appdiff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Text renders notes as "# " lines, then a CLI-style diff per resource.
func (r *Result) Text() string {
	var b strings.Builder
	for _, n := range r.Notes() {
		b.WriteString("# " + n + "\n")
	}
	for _, rd := range r.Resources {
		b.WriteString("\n" + rd.Diff)
	}
	return b.String()
}

// JSON renders the result; stat omits diffs and field changes.
func (r *Result) JSON(stat bool) ([]byte, error) {
	out := *r
	if stat {
		out.Resources = make([]ResourceDiff, len(r.Resources))
		for i, rd := range r.Resources {
			out.Resources[i] = ResourceDiff{Resource: rd.Resource, Change: rd.Change}
		}
	}
	type withNotes struct {
		Result
		Notes []string `json:"notes"`
	}
	return json.MarshalIndent(withNotes{out, r.Notes()}, "", "  ")
}

// unifiedDiff YAML-marshals both sides with sigs.k8s.io/yaml, as the CLI does.
func unifiedDiff(resource, name string, before, after *unstructured.Unstructured) (string, error) {
	a, err := marshalYAML(before)
	if err != nil {
		return "", err
	}
	b, err := marshalYAML(after)
	if err != nil {
		return "", err
	}
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(a),
		B:        difflib.SplitLines(b),
		FromFile: name + "-live.yaml",
		ToFile:   name,
		Context:  3,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("===== %s ======\n", resource) + text, nil
}

func marshalYAML(obj *unstructured.Unstructured) (string, error) {
	if obj == nil {
		return "", nil
	}
	data, err := yaml.Marshal(obj)
	return string(data), err
}

// FieldChange is one changed field, addressed by JSON Pointer.
type FieldChange struct {
	Op     string `json:"op"` // add, remove, replace
	Path   string `json:"path"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// fieldChanges lists changes from a to b. List items are paired by a key
// detected from the data, so no per-resource-type key lists are needed.
func fieldChanges(a, b any, path string) []FieldChange {
	if jsonEqual(a, b) {
		return nil
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		var out []FieldChange
		for _, k := range sorted {
			p := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			av, inA := am[k]
			bv, inB := bm[k]
			switch {
			case !inA:
				out = append(out, FieldChange{Op: "add", Path: p, After: bv})
			case !inB:
				out = append(out, FieldChange{Op: "remove", Path: p, Before: av})
			default:
				out = append(out, fieldChanges(av, bv, p)...)
			}
		}
		return out
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok {
		if key := mergeKey(bl, al); key != "" {
			old := map[string]int{}
			for i, it := range al {
				old[jsonString(it.(map[string]any)[key])] = i
			}
			var out []FieldChange
			kept := map[int]bool{}
			for i, it := range bl {
				p := fmt.Sprintf("%s/%d", path, i)
				if j, ok := old[jsonString(it.(map[string]any)[key])]; ok {
					kept[j] = true
					out = append(out, fieldChanges(al[j], it, p)...)
				} else {
					out = append(out, FieldChange{Op: "add", Path: p, After: it})
				}
			}
			for j, it := range al {
				if !kept[j] {
					out = append(out, FieldChange{Op: "remove", Path: fmt.Sprintf("%s/%d", path, j), Before: it})
				}
			}
			return out
		}
		if len(al) == len(bl) {
			var out []FieldChange
			for i := range al {
				out = append(out, fieldChanges(al[i], bl[i], fmt.Sprintf("%s/%d", path, i))...)
			}
			return out
		}
	}
	return []FieldChange{{Op: "replace", Path: path, Before: a, After: b}}
}

// mergeKey returns the scalar field, unique across items, that pairs the most
// items with others, or "" if none does.
func mergeKey(items, others []any) string {
	if len(items) == 0 {
		return ""
	}
	maps := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return ""
		}
		maps = append(maps, m)
	}
	for _, o := range others {
		if _, ok := o.(map[string]any); !ok {
			return ""
		}
	}
	candidates := []string{}
	for k := range maps[0] {
		candidates = append(candidates, k)
	}
	sort.Strings(candidates)
	best, bestMatches := "", 0
	for _, k := range candidates {
		seen := map[string]bool{}
		unique := true
		for _, m := range maps {
			v, ok := m[k]
			if !ok || isComposite(v) || seen[jsonString(v)] {
				unique = false
				break
			}
			seen[jsonString(v)] = true
		}
		if !unique {
			continue
		}
		matches := 0
		for _, o := range others {
			if v, ok := o.(map[string]any)[k]; ok && seen[jsonString(v)] {
				matches++
			}
		}
		if matches > bestMatches {
			best, bestMatches = k, matches
		}
	}
	return best
}

func isComposite(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func jsonString(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func jsonEqual(a, b any) bool {
	return jsonString(a) == jsonString(b)
}
