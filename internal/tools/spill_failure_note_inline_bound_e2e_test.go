package tools_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// When no spill file can be written, the rows that fit come back with a note
// that quotes the error, and the error names the spill directory, which comes
// from HOME or XDG_CACHE_HOME: however long that name, the note is cut in the
// middle to half the inline limit, and the rows take the rest.
func TestSpillFailureNoteStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 11_000), "pgglass")
	s := connectSpillingTo(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"app": direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)),
	}), dir)

	got := s.ok(t, "pg_query", args("app", "SELECT g, repeat('x', 100) AS pad FROM generate_series(1, 5000) g"))
	note := got[strings.LastIndex(got, "\n(")+1:]
	cut := regexp.MustCompile(`^\(\d+ of 1000 rows shown; could not write the spill file: mkdir /.+…\[\d+ bytes cut\]….+: file name too long; truncated: max_rows=1000\)$`)
	if !strings.HasPrefix(got, "g\tpad\n1\t") || !cut.MatchString(note) || len(note) > config.InlineMaxBytes/2 || len(got) > config.InlineMaxBytes {
		t.Errorf("result without a spill file: %d bytes, limit %d, note of %d:\n%.300s\n...\n%s", len(got), config.InlineMaxBytes, len(note), got, got[max(0, len(got)-200):])
	}
}
