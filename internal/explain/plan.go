package explain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// largeRows is how many rows make a relation, or the outer side of a
	// nested loop, worth a flag.
	largeRows = 10_000
	// missFactor is how many times more, or fewer, rows than planned a node
	// may return before its estimate is flagged.
	missFactor = 10
	// topNodes is how many nodes the summary ranks.
	topNodes = 5
)

// plan is the output of EXPLAIN (FORMAT JSON) for one statement.
type plan struct {
	Root node `json:"Plan"`
	// ExecutionTime, in ms, is set only with ANALYZE.
	ExecutionTime *float64 `json:"Execution Time"`
}

// node is one plan node, with the fields the summary reads. The actual
// values come with ANALYZE; time and rows are averages over Loops.
type node struct {
	Type          string  `json:"Node Type"`
	ParallelAware bool    `json:"Parallel Aware"`
	Parent        string  `json:"Parent Relationship"` // "Outer", "Inner", "SubPlan", ...
	Index         string  `json:"Index Name"`
	Schema        string  `json:"Schema"` // with VERBOSE only
	Relation      string  `json:"Relation Name"`
	Alias         string  `json:"Alias"`
	TotalCost     float64 `json:"Total Cost"`
	PlanRows      float64 `json:"Plan Rows"`
	ActualTime    float64 `json:"Actual Total Time"` // ms
	ActualRows    float64 `json:"Actual Rows"`
	Loops         float64 `json:"Actual Loops"`
	HashBatches   float64 `json:"Hash Batches"`
	SharedHit     float64 `json:"Shared Hit Blocks"` // this node and its children
	SharedRead    float64 `json:"Shared Read Blocks"`
	disk                  // the node's own, or the leader's under Gather
	Workers       []disk  `json:"Workers"` // each parallel worker's
	Plans         []node  `json:"Plans"`
}

// disk is what a sort or a hash aggregate reports about its use of disk.
type disk struct {
	SortMethod    string  `json:"Sort Method"`
	SortSpaceUsed float64 `json:"Sort Space Used"` // kB
	SortSpaceType string  `json:"Sort Space Type"` // "Disk" or "Memory"
	DiskUsage     float64 `json:"Disk Usage"`      // kB, of a hash aggregate
}

// relation names a relation by schema and name.
type relation struct{ schema, name string }

func (r relation) String() string {
	if r.schema == "" {
		return r.name
	}
	return r.schema + "." + r.name
}

// parse reads the output of EXPLAIN (FORMAT JSON) for one statement.
func parse(doc string) (*plan, error) {
	var plans []plan
	if err := json.Unmarshal([]byte(doc), &plans); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	if len(plans) != 1 {
		return nil, fmt.Errorf("read the plan: got %d plans, want 1", len(plans))
	}
	return &plans[0], nil
}

// seqScans returns the relations p scans sequentially, each once.
func (p *plan) seqScans() []relation {
	var rels []relation
	for _, n := range p.Root.all() {
		if n.Type == "Seq Scan" && n.Relation != "" && !slices.Contains(rels, n.relation()) {
			rels = append(rels, n.relation())
		}
	}
	return rels
}

// summary renders p in a few lines: the totals, the topNodes nodes that cost
// the most by themselves, and the flags. reltuples holds pg_class.reltuples
// of the relations p scans sequentially.
func (p *plan) summary(reltuples map[relation]float64) string {
	analyzed := p.ExecutionTime != nil
	nodes := p.Root.all()
	lines := []string{p.totals()}
	if analyzed {
		lines = append(lines, "top nodes by own time:")
	} else {
		lines = append(lines, "top nodes by own cost:")
	}
	for _, n := range ranked(nodes, analyzed)[:min(topNodes, len(nodes))] {
		lines = append(lines, "  "+n.label()+": "+n.measure(analyzed))
	}
	var flags []string
	for _, n := range nodes {
		for _, f := range n.flags(analyzed, reltuples) {
			flags = append(flags, "  "+n.label()+": "+f)
		}
	}
	if len(flags) == 0 {
		return strings.Join(append(lines, "flags: none"), "\n")
	}
	return strings.Join(append(append(lines, "flags:"), flags...), "\n")
}

// totals is the first line: the total cost and estimated rows or, with
// ANALYZE, the total cost, execution time, rows and shared buffers.
func (p *plan) totals() string {
	r := &p.Root
	if p.ExecutionTime == nil {
		return "total cost " + num(r.TotalCost, 2) + ", ~" + rows(r.PlanRows)
	}
	return fmt.Sprintf("total cost %s, %s ms, %s, %s buffers hit, %s read", num(r.TotalCost, 2),
		num(*p.ExecutionTime, 3), rows(r.ActualRows*r.Loops), num(r.SharedHit, 0), num(r.SharedRead, 0))
}

// all returns n and every node under it, in plan order.
func (n *node) all() []*node {
	nodes := []*node{n}
	for i := range n.Plans {
		nodes = append(nodes, n.Plans[i].all()...)
	}
	return nodes
}

// ranked returns nodes by own time or cost, the highest first; ties keep
// plan order.
func ranked(nodes []*node, analyzed bool) []*node {
	sorted := slices.Clone(nodes)
	slices.SortStableFunc(sorted, func(a, b *node) int {
		return cmp.Compare(b.own(analyzed), a.own(analyzed))
	})
	return sorted
}

// own is what n takes by itself: its total less its children's, never below
// zero. Parallel workers and the CTEs a node reads but does not run can
// make the children's sum exceed it.
func (n *node) own(analyzed bool) float64 {
	own := n.total(analyzed)
	for i := range n.Plans {
		own -= n.Plans[i].total(analyzed)
	}
	return max(own, 0)
}

// total is the time n took over all its loops with ANALYZE, otherwise its
// total cost; either includes its children's.
func (n *node) total(analyzed bool) float64 {
	if analyzed {
		return n.ActualTime * n.Loops
	}
	return n.TotalCost
}

// measure is what the ranking shows of n: its own cost and estimated rows,
// or with ANALYZE its own time and the rows of each loop.
func (n *node) measure(analyzed bool) string {
	switch {
	case !analyzed:
		return "cost " + num(n.own(false), 2) + ", ~" + rows(n.PlanRows)
	case n.Loops == 0:
		return "never executed"
	case n.Loops == 1:
		return num(n.own(true), 3) + " ms, " + rows(n.ActualRows)
	default:
		return num(n.own(true), 3) + " ms, " + rows(n.ActualRows) + " × " + num(n.Loops, 0) + " loops"
	}
}

// flags returns what is worth a look in n: an estimate off by more than
// missFactor, a seq scan of a relation of over largeRows rows, a sort or
// hash that spilled to disk, and a nested loop whose outer side returns
// over largeRows rows.
func (n *node) flags(analyzed bool, reltuples map[relation]float64) []string {
	var flags []string
	if analyzed && n.Loops > 0 && missed(n.ActualRows, n.PlanRows) {
		flags = append(flags, rows(n.ActualRows)+", planned "+num(n.PlanRows, 0))
	}
	if size := reltuples[n.relation()]; n.Type == "Seq Scan" && size > largeRows {
		flags = append(flags, "full scan of ~"+rows(size))
	}
	if spill := n.spilled(); spill != "" {
		flags = append(flags, "spilled to disk ("+spill+")")
	}
	if n.Type == "Nested Loop" {
		if outer := n.outerRows(analyzed); outer > largeRows {
			flags = append(flags, "outer side of "+rows(outer)+", each running the inner side")
		}
	}
	return flags
}

// missed reports whether actual rows are more than missFactor times the
// planned ones, or less than a missFactor-th of them. Fewer than one row
// counts as one.
func missed(actual, planned float64) bool {
	a, p := max(actual, 1), max(planned, 1)
	return a > p*missFactor || p > a*missFactor
}

// spilled describes how n, or one of its parallel workers, spilled to disk:
// a sort's method and space, a hash's batches or a hash aggregate's disk
// use. It is empty when nothing did.
func (n *node) spilled() string {
	if n.HashBatches > 1 {
		return num(n.HashBatches, 0) + " batches"
	}
	for _, d := range append([]disk{n.disk}, n.Workers...) {
		switch {
		case d.SortSpaceType == "Disk":
			return d.SortMethod + ", " + num(d.SortSpaceUsed, 0) + " kB"
		case d.DiskUsage > 0:
			return num(d.DiskUsage, 0) + " kB"
		}
	}
	return ""
}

// outerRows is how many rows the outer side of n returns: with ANALYZE the
// actual ones over all its loops, otherwise the estimate.
func (n *node) outerRows(analyzed bool) float64 {
	for i := range n.Plans {
		if outer := &n.Plans[i]; outer.Parent == "Outer" {
			if analyzed {
				return outer.ActualRows * outer.Loops
			}
			return outer.PlanRows
		}
	}
	return 0
}

func (n *node) relation() relation {
	return relation{schema: n.Schema, name: n.Relation}
}

// label names n as EXPLAIN's text format does, e.g.
// "Index Scan using orders_pkey on app.orders o".
func (n *node) label() string {
	label := n.Type
	if n.ParallelAware {
		label = "Parallel " + label
	}
	if n.Index != "" {
		label += " using " + n.Index
	}
	if n.Relation != "" {
		label += " on " + n.relation().String()
		if n.Alias != "" && n.Alias != n.Relation {
			label += " " + n.Alias
		}
	}
	return label
}

// num formats v with at most decimals digits after the point.
func num(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// rows formats a row count: "1 row", "5000 rows".
func rows(v float64) string {
	if s := num(v, 0); s != "1" {
		return s + " rows"
	}
	return "1 row"
}
