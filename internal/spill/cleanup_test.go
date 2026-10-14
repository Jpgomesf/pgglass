package spill

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type staleInfo struct{ name string }

func (i staleInfo) Name() string       { return i.name }
func (i staleInfo) Size() int64        { return 1 }
func (i staleInfo) Mode() fs.FileMode  { return 0o600 }
func (i staleInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i staleInfo) IsDir() bool        { return false }
func (i staleInfo) Sys() any           { return nil }

func TestRemoveIfStaleReportsFailures(t *testing.T) {
	entry := fs.FileInfoToDirEntry(staleInfo{"spill-pg_query-prod-20261010T000000Z-1.csv"})
	cutoff := time.Now()

	if err := removeIfStale(t.TempDir(), entry, cutoff); err != nil {
		t.Errorf("a file already removed, as by a concurrent Cleanup, is not an error: %v", err)
	}

	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfStale(notDir, entry, cutoff); err == nil {
		t.Error("want the error of a failed removal")
	}
}
