// Package spill keeps tool output small: a result too large to return inline
// is written to a file, and the tool returns a short summary instead.
package spill

import (
	"errors"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

var errNotImplemented = errors.New("not implemented")

// Store writes oversized outputs under Dir and removes them after TTL.
type Store struct {
	Dir            string           // created on demand
	TTL            time.Duration    // spill files older than this are removed by Cleanup
	InlineMaxBytes int              // rendered outputs up to this size are returned inline
	Now            func() time.Time // clock; nil means time.Now
}

// Output renders res in format f. A rendering of at most InlineMaxBytes is
// returned as is. A larger one is written to a new file under Dir, whose
// unique name includes tool and conn (made safe for file names), and Output
// returns a short summary (row and column counts, column names, the file
// path) instead.
func (s *Store) Output(tool, conn string, res *result.Result, f render.Format) (string, error) {
	return "", errNotImplemented
}

// Cleanup removes spill files in Dir older than TTL. It never touches files
// it did not create.
func (s *Store) Cleanup() error {
	return errNotImplemented
}
