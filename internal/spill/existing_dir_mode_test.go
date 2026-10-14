package spill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/spill"
)

// The README's Docker step makes the spill directory with mkdir -p, which the
// usual umask 022 leaves at 0755, so other local users who can reach it could
// list the spill files: the directory a spill file goes to is 0700, whoever
// made it.
func TestSpillDirectoryThatExistsIsMadePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".cache", "pgglass")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res := result.Text("pad")
	for range 500 {
		res.AddRow(strings.Repeat("x", 100))
	}
	store := &spill.Store{Dir: dir, InlineMaxBytes: 10_000}

	out, err := store.OutputWithin("pg_query", "db", res, render.TSV, 10_000)
	if err != nil || !strings.Contains(out, "Full result: "+dir+string(filepath.Separator)) {
		t.Fatalf("OutputWithin = %v:\n%.300s", err, out)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("spill directory mode %v after a spill, want 0700", mode)
	}
}
