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
	largeRows  = 10_000
	missFactor = 10
	topNodes   = 5
)

type plan struct {
	Root node `json:"Plan"`
	// ExecutionTime, in ms, is set only with ANALYZE.
	ExecutionTime *float64 `json:"Execution Time"`
}

type node struct {
	Type          string  `json:"Node Type"`
	Strategy      string  `json:"Strategy"`
	ParallelAware bool    `json:"Parallel Aware"`
	Parent        string  `json:"Parent Relationship"`
	Subplan       string  `json:"Subplan Name"`
	Index         string  `json:"Index Name"`
	Schema        string  `json:"Schema"`
	Relation      string  `json:"Relation Name"`
	Alias         string  `json:"Alias"`
	StartupCost   float64 `json:"Startup Cost"`
	TotalCost     float64 `json:"Total Cost"`
	PlanRows      float64 `json:"Plan Rows"`
	Width         float64 `json:"Plan Width"`
	ActualTime    float64 `json:"Actual Total Time"`
	ActualRows    float64 `json:"Actual Rows"`
	Loops         float64 `json:"Actual Loops"`
	Filtered      float64 `json:"Rows Removed by Filter"`
	HashBatches   float64 `json:"Hash Batches"`
	SharedHit     float64 `json:"Shared Hit Blocks"`
	SharedRead    float64 `json:"Shared Read Blocks"`
	disk
	Workers []disk `json:"Workers"`
	Plans   []node `json:"Plans"`
	share   float64
}

type disk struct {
	SortMethod    string  `json:"Sort Method"`
	SortSpaceUsed float64 `json:"Sort Space Used"`
	SortSpaceType string  `json:"Sort Space Type"`
	DiskUsage     float64 `json:"Disk Usage"`
	Storage       string  `json:"Storage"`
	MaxStorage    float64 `json:"Maximum Storage"`
}

type relation struct{ schema, name string }

func (r relation) String() string {
	if r.schema == "" {
		return r.name
	}
	return r.schema + "." + r.name
}

func parse(doc string) (*plan, error) {
	var plans []plan
	if err := json.Unmarshal([]byte(doc), &plans); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	if len(plans) != 1 {
		return nil, fmt.Errorf("read the plan: got %d plans, want 1", len(plans))
	}
	plans[0].Root.setShare(1)
	return &plans[0], nil
}

func (n *node) setShare(share float64) {
	n.share = share
	for i := range n.Plans {
		c := &n.Plans[i]
		switch {
		case c.Parent == "InitPlan" || c.Parent == "SubPlan":
			c.setShare(c.subplanShare())
		case !n.streams(c):
			c.setShare(1)
		case n.Type == "Limit":
			c.setShare(n.limitShare(c, share))
		default:
			c.setShare(share)
		}
	}
}

func (n *node) streams(c *node) bool {
	switch {
	case n.Type == "Sort", n.Type == "Hash", n.Type == "Bitmap Heap Scan", n.Strategy != "" && n.Strategy != "Sorted":
		return false
	case n.Type == "WindowAgg" && n.ownStartupCost() >= c.TotalCost:
		return false
	case c.Parent == "Inner":
		return n.Type == "Merge Join"
	default:
		return c.Parent != "InitPlan" && c.Parent != "SubPlan"
	}
}

func (n *node) ownStartupCost() float64 {
	cost := n.StartupCost
	for i := range n.Plans {
		if c := &n.Plans[i]; c.Parent == "InitPlan" {
			cost -= c.StartupCost + c.subplanShare()*(c.TotalCost-c.StartupCost)
		}
	}
	return cost
}

func (n *node) subplanShare() float64 {
	if n.Width == 0 && !strings.HasPrefix(n.Subplan, "CTE ") {
		return 1 / max(n.PlanRows, 1)
	}
	return 1
}

func (n *node) limitShare(c *node, share float64) float64 {
	read := share * n.PlanRows / max(c.PlanRows, 1)
	if run := c.TotalCost - c.StartupCost; run > 0 {
		read += max(n.ownStartupCost()-c.StartupCost, 0) / run
	}
	return min(read, 1)
}

func (p *plan) seqScans() []relation {
	var rels []relation
	for _, n := range p.Root.all() {
		if n.Type == "Seq Scan" && n.Relation != "" && !slices.Contains(rels, n.relation()) {
			rels = append(rels, n.relation())
		}
	}
	return rels
}

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

func (p *plan) totals() string {
	r := &p.Root
	if p.ExecutionTime == nil {
		return "total cost " + num(r.TotalCost, 2) + ", ~" + rows(r.PlanRows, 0)
	}
	return fmt.Sprintf("total cost %s, %s ms, %s, %s buffers hit, %s read", num(r.TotalCost, 2),
		num(*p.ExecutionTime, 3), rows(r.ActualRows*r.Loops, 0), num(r.SharedHit, 0), num(r.SharedRead, 0))
}

func (n *node) all() []*node {
	nodes := []*node{n}
	for i := range n.Plans {
		nodes = append(nodes, n.Plans[i].all()...)
	}
	return nodes
}

func ranked(nodes []*node, analyzed bool) []*node {
	sorted := slices.Clone(nodes)
	slices.SortStableFunc(sorted, func(a, b *node) int {
		return cmp.Compare(b.own(analyzed), a.own(analyzed))
	})
	return sorted
}

func (n *node) own(analyzed bool) float64 {
	own := n.total(analyzed)
	for i := range n.Plans {
		own -= n.Plans[i].total(analyzed)
	}
	return max(own, 0)
}

func (n *node) total(analyzed bool) float64 {
	if analyzed {
		return n.ActualTime * n.Loops
	}
	return n.TotalCost - (1-n.share)*(n.TotalCost-n.StartupCost)
}

func (n *node) measure(analyzed bool) string {
	switch {
	case !analyzed:
		return "cost " + num(n.own(false), 2) + ", ~" + rows(n.PlanRows*n.share, 0)
	case n.Loops == 0:
		return "never executed"
	case n.Loops == 1:
		return num(n.own(true), 3) + " ms, " + rows(n.ActualRows, 2)
	default:
		return num(n.own(true), 3) + " ms, " + rows(n.ActualRows, 2) + " × " + num(n.Loops, 0) + " loops"
	}
}

func (n *node) flags(analyzed bool, reltuples map[relation]float64) []string {
	var flags []string
	if analyzed && n.Loops > 0 && missed(n.ActualRows, n.PlanRows, n.share < 1) {
		flags = append(flags, rows(n.ActualRows, 2)+", planned "+num(n.PlanRows, 0))
	}
	if size := reltuples[n.relation()]; n.Type == "Seq Scan" && size > largeRows && n.read(analyzed, size) > largeRows {
		flags = append(flags, "full scan of ~"+rows(size, 0))
	}
	if spill := n.spilled(); spill != "" {
		flags = append(flags, "spilled to disk ("+spill+")")
	}
	if n.Type == "Nested Loop" {
		if outer := n.outerRows(analyzed); outer > largeRows {
			flags = append(flags, "outer side of "+rows(outer, 0)+", each running the inner side")
		}
	}
	return flags
}

func missed(actual, planned float64, limited bool) bool {
	a, p := max(actual, 1), max(planned, 1)
	return a > p*missFactor || (!limited && p > a*missFactor)
}

func (n *node) read(analyzed bool, size float64) float64 {
	if analyzed {
		return (n.ActualRows + n.Filtered) * n.Loops
	}
	return size * n.share
}

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
		case d.Storage == "Disk" && n.Loops > 0:
			return num(d.MaxStorage, 0) + " kB"
		}
	}
	return ""
}

func (n *node) outerRows(analyzed bool) float64 {
	for i := range n.Plans {
		if outer := &n.Plans[i]; outer.Parent == "Outer" {
			if analyzed {
				return outer.ActualRows * outer.Loops
			}
			return outer.PlanRows * outer.share
		}
	}
	return 0
}

func (n *node) relation() relation {
	return relation{schema: n.Schema, name: n.Relation}
}

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

func num(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func rows(v float64, decimals int) string {
	if s := num(v, decimals); s != "1" {
		return s + " rows"
	}
	return "1 row"
}
