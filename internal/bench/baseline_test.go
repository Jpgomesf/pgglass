package bench

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

const (
	baselinePath     = "testdata/bench-baseline.json"
	docsPath         = "../../docs/benchmark.md"
	toolsList        = "tools/list"
	instructions     = "instructions"
	toolsListCap     = 6000
	maxGrowthPercent = 10
	tableHeader      = "| Scenario | Bytes | Tokens (est.) |\n|---|---:|---:|\n"
	baselineNote     = "Bytes each scenario puts in an agent's context, and tokens estimated as bytes/4. " +
		"See docs/benchmark.md; make bench-baseline rewrites this file."
)

var docsTable = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tableHeader) + `(\|.*\n)*`)

type measurement struct {
	Name      string `json:"name"`
	Bytes     int    `json:"bytes"`
	TokensEst int    `json:"tokens_est"`
}

type baseline struct {
	Note      string        `json:"note"`
	Image     string        `json:"postgres_image"`
	Scenarios []measurement `json:"scenarios"`
}

func newMeasurement(name string, bytes int) measurement {
	return measurement{Name: name, Bytes: bytes, TokensEst: tokens(bytes)}
}

func tokens(bytes int) int {
	return (bytes + 3) / 4
}

func regressions(want, got []measurement) []string {
	before := make(map[string]int, len(want))
	for _, m := range want {
		before[m.Name] = m.Bytes
	}
	var problems []string
	for _, m := range got {
		base, ok := before[m.Name]
		delete(before, m.Name)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: not in %s", m.Name, baselinePath))
		case m.Bytes*100 > base*(100+maxGrowthPercent):
			problems = append(problems, fmt.Sprintf("%s: %d bytes, %.1f%% more than the baseline's %d (at most %d%%)",
				m.Name, m.Bytes, 100*float64(m.Bytes-base)/float64(base), base, maxGrowthPercent))
		}
		if m.Name == toolsList && m.Bytes > toolsListCap {
			problems = append(problems, fmt.Sprintf("%s: %d bytes, over its cap of %d", toolsList, m.Bytes, toolsListCap))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		problems = append(problems, fmt.Sprintf("%s: in %s but not measured", name, baselinePath))
	}
	return problems
}

func table(ms []measurement) string {
	var b strings.Builder
	b.WriteString(tableHeader)
	total := 0
	for _, m := range ms {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", m.Name, m.Bytes, m.TokensEst)
		total += m.Bytes
	}
	fmt.Fprintf(&b, "| total | %d | %d |\n", total, tokens(total))
	return b.String()
}

func withTable(doc, table string) (string, error) {
	loc := docsTable.FindStringIndex(doc)
	if loc == nil {
		return "", fmt.Errorf("%s has no table of numbers to replace", docsPath)
	}
	return doc[:loc[0]] + table + doc[loc[1]:], nil
}

func load(t *testing.T) []measurement {
	t.Helper()
	var b baseline
	if err := json.Unmarshal([]byte(read(t, baselinePath)), &b); err != nil {
		t.Fatalf("%s: %v", baselinePath, err)
	}
	return b.Scenarios
}

func save(t *testing.T, ms []measurement) {
	t.Helper()
	data, err := json.MarshalIndent(baseline{Note: baselineNote, Image: testdb.Image(), Scenarios: ms}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := withTable(read(t, docsPath), table(ms))
	if err != nil {
		t.Fatal(err)
	}
	write(t, baselinePath, string(data)+"\n")
	write(t, docsPath, doc)
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRegressions checks what fails the benchmark: a scenario more than 10%
// larger than the baseline, one the baseline lacks or holds but that is no
// longer measured, and tools/list over its cap.
func TestRegressions(t *testing.T) {
	want := []measurement{newMeasurement(toolsList, 5800), newMeasurement("query", 1000)}
	for _, tc := range []struct {
		name string
		got  []measurement
		want []string
	}{
		{"same", want, nil},
		{"10% larger", []measurement{newMeasurement(toolsList, 5900), newMeasurement("query", 1100)}, nil},
		{"smaller", []measurement{newMeasurement(toolsList, 10), newMeasurement("query", 0)}, nil},
		{"over 10% larger", []measurement{newMeasurement(toolsList, 5800), newMeasurement("query", 1101)},
			[]string{"query: 1101 bytes, 10.1% more than the baseline's 1000 (at most 10%)"}},
		{"tools/list over its cap", []measurement{newMeasurement(toolsList, 6001), newMeasurement("query", 1000)},
			[]string{"tools/list: 6001 bytes, over its cap of 6000"}},
		{"new scenario", append(slices.Clone(want), newMeasurement("explain", 1)),
			[]string{"explain: not in testdata/bench-baseline.json"}},
		{"scenario gone", want[:1], []string{"query: in testdata/bench-baseline.json but not measured"}},
	} {
		if got := regressions(want, tc.got); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTokensAreBytesOverFourRoundedUp checks the estimate of tokens.
func TestTokensAreBytesOverFourRoundedUp(t *testing.T) {
	for bytes, want := range map[int]int{0: 0, 1: 1, 4: 1, 5: 2, 6000: 1500} {
		if got := tokens(bytes); got != want {
			t.Errorf("tokens(%d) = %d, want %d", bytes, got, want)
		}
	}
}

// TestShownHidesWhereTheTestSpills checks that sizes do not depend on the
// test's temporary directory or the random part of a spill file's name.
func TestShownHidesWhereTheTestSpills(t *testing.T) {
	const dir = "/tmp/TestTokenBudget4242/001"
	for text, want := range map[string]string{
		"Full result: " + dir + "/spill-pg_query-prod-app-20261013T190102Z-42.csv\n(1 row)": "Full result: " +
			"/home/me/.cache/pgglass/spill-pg_query-prod-app-20261013T190102Z-4294967295.csv\n(1 row)",
		"id\n42\n(1 row)": "id\n42\n(1 row)",
	} {
		if got := shown(text, dir); got != want {
			t.Errorf("shown(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestWithTableReplacesTheTableOfNumbers checks the rewrite of the table in
// docs/benchmark.md.
func TestWithTableReplacesTheTableOfNumbers(t *testing.T) {
	old := table([]measurement{newMeasurement("query", 100)})
	doc := "# Benchmark\n\n" + old + "\nRegenerate it.\n"
	got, err := withTable(doc, table([]measurement{newMeasurement("query", 90), newMeasurement("explain", 20)}))
	want := "# Benchmark\n\n" + tableHeader + "| query | 90 | 23 |\n| explain | 20 | 5 |\n| total | 110 | 28 |\n\nRegenerate it.\n"
	if err != nil || got != want {
		t.Errorf("withTable = %q, %v; want %q", got, err, want)
	}
	if _, err := withTable("# Benchmark\n", old); err == nil {
		t.Error("withTable on a doc without a table: no error")
	}
}

// TestDocsShowTheBaseline checks that docs/benchmark.md shows the numbers of
// the baseline.
func TestDocsShowTheBaseline(t *testing.T) {
	if !strings.Contains(read(t, docsPath), table(load(t))) {
		t.Errorf("%s does not show the numbers of %s: run make bench-baseline", docsPath, baselinePath)
	}
}
