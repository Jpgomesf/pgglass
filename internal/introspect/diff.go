package introspect

import (
	"maps"
	"slices"
)

// Diff lists the differences from left to right in name order, one per line:
// "- " only in left, "+ " only in right, "~ " changed, as left → right.
func Diff(left, right SchemaStructure) []string {
	var lines []string
	for _, name := range unionKeys(left, right) {
		l, inLeft := left[name]
		r, inRight := right[name]
		switch {
		case !inRight:
			lines = append(lines, "- "+l.Kind+" "+name)
		case !inLeft:
			lines = append(lines, "+ "+r.Kind+" "+name)
		default:
			lines = append(lines, diffRelation(name, l, r)...)
		}
	}
	return lines
}

func diffRelation(name string, l, r RelationStructure) []string {
	var lines []string
	if a, b := l.summary(), r.summary(); a != b {
		lines = append(lines, "~ relation "+name+": "+a+" → "+b)
	}
	if l.Definition != "" && r.Definition != "" && l.Definition != r.Definition {
		lines = append(lines, "~ "+l.Kind+" "+name+": "+oneLine(l.Definition)+" → "+oneLine(r.Definition))
	}
	lines = append(lines, diffObjects("column", name, l.Columns, r.Columns)...)
	lines = append(lines, diffObjects("constraint", name, l.Constraints, r.Constraints)...)
	lines = append(lines, diffObjects("index", name, l.Indexes, r.Indexes)...)
	return append(lines, diffObjects("policy", name, l.Policies, r.Policies)...)
}

func (r RelationStructure) summary() string {
	if r.RLS {
		return r.Kind + " with RLS"
	}
	return r.Kind
}

func diffObjects(kind, rel string, left, right map[string]string) []string {
	var lines []string
	for _, name := range unionKeys(left, right) {
		l, inLeft := left[name]
		r, inRight := right[name]
		label := kind + " " + rel + "." + name
		switch {
		case !inRight:
			lines = append(lines, "- "+label)
		case !inLeft:
			lines = append(lines, "+ "+label)
		case l != r:
			lines = append(lines, "~ "+label+": "+oneLine(l)+" → "+oneLine(r))
		}
	}
	return lines
}

func unionKeys[V any](a, b map[string]V) []string {
	keys := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(keys)
	return slices.Compact(keys)
}
