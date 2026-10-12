package guard

import (
	"strings"
	"testing"
	"unicode/utf8"

	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// nested builds SQL that nests n levels of one construct.
var nested = map[string]func(n int) string{
	"operator":        func(n int) string { return "SELECT 1" + strings.Repeat(" + 1", n) },
	"unary minus":     func(n int) string { return "SELECT " + strings.Repeat("- ", n) + "x" },
	"NOT":             func(n int) string { return "SELECT " + strings.Repeat("NOT ", n) + "true" },
	"cast":            func(n int) string { return "SELECT 1" + strings.Repeat("::foo", n) },
	"COLLATE":         func(n int) string { return "SELECT x" + strings.Repeat(` COLLATE "C"`, n) },
	"json arrow":      func(n int) string { return "SELECT x" + strings.Repeat("->'a'", n) },
	"IS NULL":         func(n int) string { return "SELECT x" + strings.Repeat(" IS NULL", n) },
	"AT TIME ZONE":    func(n int) string { return "SELECT x" + strings.Repeat(" AT TIME ZONE 'UTC'", n) },
	"function":        func(n int) string { return "SELECT " + strings.Repeat("abs(", n) + "1" + strings.Repeat(")", n) },
	"named argument":  func(n int) string { return "SELECT " + strings.Repeat("f(a => ", n) + "1" + strings.Repeat(")", n) },
	"scalar subquery": func(n int) string { return "SELECT " + strings.Repeat("(SELECT ", n) + "1" + strings.Repeat(")", n) },
	"EXISTS": func(n int) string {
		return "SELECT " + strings.Repeat("EXISTS (SELECT ", n) + "1" + strings.Repeat(")", n)
	},
	"IN subquery": func(n int) string {
		return "SELECT 1 WHERE " + strings.Repeat("1 IN (SELECT ", n) + "1" + strings.Repeat(")", n)
	},
	"FROM subquery": func(n int) string {
		return strings.Repeat("SELECT * FROM (", n) + "SELECT 1" + strings.Repeat(") s", n)
	},
	"WITH": func(n int) string {
		return strings.Repeat("WITH a AS (", n) + "SELECT 1" + strings.Repeat(") SELECT 1", n)
	},
	"subscript": func(n int) string { return "SELECT " + strings.Repeat("a[", n) + "1" + strings.Repeat("]", n) },
	"ARRAY":     func(n int) string { return "SELECT " + strings.Repeat("ARRAY[", n) + "1" + strings.Repeat("]", n) },
	"ROW":       func(n int) string { return "SELECT " + strings.Repeat("ROW(", n) + "1" + strings.Repeat(")", n) },
	"CASE": func(n int) string {
		return "SELECT " + strings.Repeat("CASE WHEN true THEN ", n) + "1" + strings.Repeat(" END", n)
	},
	"JOIN":            func(n int) string { return "SELECT 1 FROM a" + strings.Repeat(" JOIN a ON true", n) },
	"UNION":           func(n int) string { return "SELECT 1" + strings.Repeat(" UNION SELECT 1", n) },
	"field selection": func(n int) string { return "SELECT " + strings.Repeat("(", n) + "x" + strings.Repeat(").a", n) },
	"JSON_OBJECT": func(n int) string {
		return "SELECT " + strings.Repeat("JSON_OBJECT('a': ", n) + "1" + strings.Repeat(")", n)
	},
	"window": func(n int) string {
		return "SELECT " + strings.Repeat("sum(x) OVER (ORDER BY ", n) + "1" + strings.Repeat(")", n)
	},
	"VALUES": func(n int) string { return strings.Repeat("VALUES ((", n) + "1" + strings.Repeat("))", n) },
	"JSON_TABLE NESTED": func(n int) string {
		return "SELECT * FROM JSON_TABLE(j, '$' COLUMNS (" + strings.Repeat("NESTED PATH '$' COLUMNS (", n) + "a int" + strings.Repeat(")", n) + "))"
	},
}

// treeDepth is the depth of m in protobuf messages, as the decoder counts it.
func treeDepth(m protoreflect.Message) int {
	deepest := 0
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Message() == nil:
		case fd.IsList():
			for i := range v.List().Len() {
				deepest = max(deepest, treeDepth(v.List().Get(i).Message()))
			}
		default:
			deepest = max(deepest, treeDepth(v.Message()))
		}
		return true
	})
	return deepest + 1
}

func TestNestingBoundIsAnUpperBound(t *testing.T) {
	for name, build := range nested {
		for _, n := range []int{1, 7, 300} {
			sql := build(n)
			bound, err := nestingBound(sql)
			if err != nil {
				t.Fatalf("%s x %d: nestingBound: %v", name, n, err)
			}
			// 300 levels are safe to parse whatever the bound says.
			tree, err := pgquery.Parse(sql)
			if err != nil {
				t.Fatalf("%s x %d: Parse: %v", name, n, err)
			}
			if depth := treeDepth(tree.ProtoReflect()); depth > bound {
				t.Errorf("%s x %d: nestingBound = %d, but the parse tree is %d deep", name, n, bound, depth)
			}
		}
	}
}

func TestNestingBoundRejectsWhatWouldOverflowTheParser(t *testing.T) {
	// The shallowest depths at which the parser's stack overflowed on
	// linux/amd64 when this was written; arm64 needed about twice as many.
	for name, n := range map[string]int{
		"operator":        3_500,
		"unary minus":     3_500,
		"NOT":             3_500,
		"cast":            3_500,
		"IS NULL":         3_500,
		"COLLATE":         5_000,
		"json arrow":      5_000,
		"AT TIME ZONE":    3_500,
		"field selection": 3_500,
		"function":        3_500,
		"named argument":  2_000,
		"scalar subquery": 1_500,
		"EXISTS":          1_500,
		"IN subquery":     1_500,
		"subscript":       2_500,
		"ARRAY":           3_500,
		"ROW":             3_500,
		"JOIN":            5_000,
		"JSON_OBJECT":     2_000,
		"VALUES":          1_500,
	} {
		bound, err := nestingBound(nested[name](n))
		if err != nil || bound <= 2*maxNesting {
			t.Errorf("%s x %d: nestingBound = %d, %v; want more than twice %d", name, n, bound, err, maxNesting)
		}
	}
}

func TestNestingBoundWeighsTokens(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"", baseWeight},
		{"SELECT 1 -- note", opWeight + baseWeight},
		// A comma ends an element: the two calls are siblings.
		{"SELECT f(1), g(2)", opWeight + groupWeight + baseWeight},
		{"SELECT f(g(1))", opWeight + 2*groupWeight + baseWeight},
		// Unbalanced brackets, which the parser then rejects.
		{"SELECT ((((", opWeight + 4*groupWeight + baseWeight},
		{"SELECT 1))))", 5*opWeight + baseWeight},
	} {
		if got, err := nestingBound(c.sql); err != nil || got != c.want {
			t.Errorf("nestingBound(%q) = %d, %v; want %d", c.sql, got, err, c.want)
		}
	}
}

// FuzzNestingBound: whenever Check would parse SQL, its tree is no deeper
// than nestingBound says.
func FuzzNestingBound(f *testing.F) {
	for _, build := range nested {
		f.Add(build(3))
	}
	f.Fuzz(func(t *testing.T, sql string) {
		bound, err := nestingBound(sql)
		if err != nil || bound > maxNesting || strings.ContainsRune(sql, 0) || !utf8.ValidString(sql) {
			return // Check never parses this, and the parser could overflow on it
		}
		tree, err := pgquery.Parse(sql)
		if err != nil {
			return
		}
		if depth := treeDepth(tree.ProtoReflect()); depth > bound {
			t.Fatalf("nestingBound(%q) = %d, but the parse tree is %d deep", sql, bound, depth)
		}
	})
}
