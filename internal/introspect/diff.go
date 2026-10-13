package introspect

import (
	"maps"
	"slices"
)

// Diff returns the differences between the structures left and right, one
// line each: "- " starts what only left has, "+ " what only right has and
// "~ " what both define differently, followed by the left definition, an
// arrow and the right one:
//
//	~ column orders.total: numeric(12,2) NOT NULL → numeric(14,2) NOT NULL
//	+ index orders.orders_placed_at_idx
//	- table tenant_notes
//
// Relations come in name order, each followed by the differences in its
// query, when both sides have one, then in its columns, constraints, indexes
// and policies, each in name order. A relation only one side has takes one
// line: what is in it is not listed. Diff returns no lines when the
// structures match.
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

// diffRelation compares relation name, which both sides have: its kind and
// row-level security, its query when both sides are views or materialized
// views (the line names the left side's kind), then the objects in it.
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

// summary is the kind of the relation and whether row-level security is on.
func (r RelationStructure) summary() string {
	if r.RLS {
		return r.Kind + " with RLS"
	}
	return r.Kind
}

// diffObjects compares the objects of one kind in relation rel, by name.
// Definitions show on one line each.
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

// unionKeys returns the keys of a and b in byte order, each once.
func unionKeys[V any](a, b map[string]V) []string {
	keys := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(keys)
	return slices.Compact(keys)
}
