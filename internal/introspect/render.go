package introspect

import (
	"fmt"
	"regexp"
	"strings"
)

// Render returns one line per schema: name, relation count and comment.
func (l SchemaList) Render() string {
	if len(l) == 0 {
		return "no schemas the current role can use"
	}
	lines := make([]string, len(l))
	for i, s := range l {
		lines[i] = s.Name + ": " + count(int64(s.Relations), "relation") + comment(s.Comment)
	}
	return strings.Join(lines, "\n")
}

// Render returns one line per relation, e.g.
// "app.orders: table, ~50 rows, 48 kB, RLS -- comment".
func (l RelationList) Render() string {
	if len(l) == 0 {
		return "no relations the current role can read"
	}
	lines := make([]string, len(l))
	for i, r := range l {
		lines[i] = r.line()
	}
	return strings.Join(lines, "\n")
}

func (r Relation) line() string {
	facts := []string{r.Kind}
	if r.Rows >= 0 {
		facts = append(facts, "~"+count(r.Rows, "row"))
	}
	if r.Size != "" {
		facts = append(facts, r.Size)
	}
	if r.RLS {
		facts = append(facts, "RLS")
	}
	return r.Schema + "." + r.Name + ": " + strings.Join(facts, ", ") + comment(r.Comment)
}

// Render returns the description in a compact, DDL-like form; an index that
// implements a constraint shows only as the constraint.
func (t TableDetail) Render() string {
	lines := []string{t.line()}
	for _, c := range t.Columns {
		lines = append(lines, "  "+c.line())
	}
	lines = appendSection(lines, "constraints:", t.constraintLines())
	lines = appendSection(lines, "indexes:", t.indexLines())
	lines = appendSection(lines, "policies:", t.policyLines())
	if len(t.Triggers) > 0 {
		lines = append(lines, "triggers: "+strings.Join(t.Triggers, ", "))
	}
	if t.Definition != "" {
		lines = append(lines, "definition:", t.Definition)
	}
	return strings.Join(lines, "\n")
}

func (c Column) line() string {
	s := c.Name + " " + c.Type
	if c.NotNull {
		s += " NOT NULL"
	}
	if c.Default != "" {
		s += " DEFAULT " + oneLine(c.Default)
	}
	if c.Generated != "" {
		s += " " + oneLine(c.Generated)
	}
	var notes []string
	if c.Comment != "" {
		notes = append(notes, oneLine(c.Comment))
	}
	if len(c.EnumValues) > 0 {
		notes = append(notes, "enum: "+quoteAll(c.EnumValues))
	}
	if len(notes) > 0 {
		s += " -- " + strings.Join(notes, "; ")
	}
	return s
}

func (t TableDetail) constraintLines() []string {
	lines := make([]string, len(t.Constraints))
	for i, c := range t.Constraints {
		lines[i] = c.Name + " " + oneLine(c.Definition)
	}
	return lines
}

func (t TableDetail) indexLines() []string {
	constraints := make(map[string]bool, len(t.Constraints))
	for _, c := range t.Constraints {
		constraints[c.Name] = true
	}
	var lines []string
	for _, ix := range t.Indexes {
		if !constraints[ix.Name] {
			lines = append(lines, oneLine(ix.Definition))
		}
	}
	return lines
}

func (t TableDetail) policyLines() []string {
	lines := make([]string, len(t.Policies))
	for i, p := range t.Policies {
		lines[i] = p.line()
	}
	return lines
}

func (p Policy) line() string {
	s := p.Name
	if !p.Permissive {
		s += " AS RESTRICTIVE"
	}
	s += " FOR " + p.Command + " TO " + strings.Join(p.Roles, ", ")
	if p.Using != "" {
		s += " USING (" + oneLine(p.Using) + ")"
	}
	if p.WithCheck != "" {
		s += " WITH CHECK (" + oneLine(p.WithCheck) + ")"
	}
	return s
}

// Render returns Lines joined by newlines, or "no matches".
func (l MatchList) Render() string {
	if len(l) == 0 {
		return "no matches"
	}
	return strings.Join(l.Lines(), "\n")
}

// Lines returns one line per match: the qualified name, then the column
// type, relation kind or function kind.
func (l MatchList) Lines() []string {
	lines := make([]string, len(l))
	for i, m := range l {
		name := m.Schema + "." + m.Table
		if m.Column != "" {
			name += "." + m.Column
		}
		lines[i] = name + ": " + m.Type
	}
	return lines
}

func appendSection(lines []string, title string, section []string) []string {
	if len(section) == 0 {
		return lines
	}
	lines = append(lines, title)
	for _, s := range section {
		lines = append(lines, "  "+s)
	}
	return lines
}

func count(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func comment(s string) string {
	if s == "" {
		return ""
	}
	return " -- " + oneLine(s)
}

var lineBreaks = regexp.MustCompile(`\s*\n\s*`)

func oneLine(s string) string {
	return lineBreaks.ReplaceAllString(strings.TrimSpace(s), " ")
}

func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = quote(v)
	}
	return strings.Join(quoted, ", ")
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
