// Package spill keeps tool output small: a result too large to return inline
// is written to a CSV file, and the tool returns a short summary instead.
package spill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

const (
	inlineMaxCell   = 200 // characters Output keeps per cell inline
	cleanupInterval = time.Hour
	stampLayout     = "20060102T150405Z"
	maxNamePart     = 32 // bytes kept of the tool and connection names in a file name
)

// spillName matches the names Output gives its files:
// spill-<tool>-<conn>-<UTC timestamp>-<random>.csv.
var spillName = regexp.MustCompile(`^spill-[A-Za-z0-9_-]*-[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9]+\.csv$`)

// Store writes oversized outputs under Dir and removes them after TTL. It is
// safe for concurrent use and must not be copied after first use.
type Store struct {
	Dir            string           // created on demand
	TTL            time.Duration    // spill files older than this are removed by Cleanup; <= 0 keeps them
	InlineMaxBytes int              // rendered outputs up to this size are returned inline
	Now            func() time.Time // clock; nil means time.Now

	mu          sync.Mutex
	lastCleanup time.Time
}

// Output renders res in format f for a tool response. The inline rendering
// cuts cells to 200 characters and ends with a footer that counts the rows
// and says when res was truncated, as in "(1000 rows; truncated:
// max_rows=1000)". When it fits in InlineMaxBytes, Output returns it.
//
// Otherwise Output writes the full result as CSV to a new file under Dir,
// named spill-<tool>-<conn>-<timestamp>-<random>.csv with tool and conn made
// safe for file names, and returns a summary instead: row and column counts,
// the truncation flag and the file path, then the first 20 columns with
// their types and the first 5 rows, as many of each as fit in
// InlineMaxBytes. If the file cannot be written, Output returns as many
// leading rows as fit in InlineMaxBytes, with a note saying why.
//
// Output runs Cleanup at most once an hour, ignoring its errors.
func (s *Store) Output(tool, conn string, res *result.Result, f render.Format) (string, error) {
	return s.OutputWithin(tool, conn, res, f, s.InlineMaxBytes)
}

// OutputWithin is Output with limit in place of InlineMaxBytes, for an
// output that shares the inline space with others.
func (s *Store) OutputWithin(tool, conn string, res *result.Result, f render.Format, limit int) (string, error) {
	return s.output(tool, conn, res, f, limit, inlineMaxCell)
}

// OutputWhole is Output without the cut of inline cells, for a result of a
// few rows whose every value counts, such as pg_profile's.
func (s *Store) OutputWhole(tool, conn string, res *result.Result, f render.Format) (string, error) {
	return s.output(tool, conn, res, f, s.InlineMaxBytes, 0)
}

// Spill writes res to a file and returns its summary, as Output does when
// res does not fit. It is for output its caller found too large to return
// inline, which Output, cutting its cells, could still return inline without
// the ends of its long values.
func (s *Store) Spill(tool, conn string, res *result.Result, f render.Format) (string, error) {
	s.maybeCleanup()
	inline, err := inlineText(res, f, 0)
	if err != nil {
		return "", err
	}
	return s.spill(tool, conn, res, f, len(inline), s.InlineMaxBytes)
}

// output returns res inline, its cells cut to maxCell characters (0 keeps
// them whole), when that fits in limit bytes, and spills it otherwise.
func (s *Store) output(tool, conn string, res *result.Result, f render.Format, limit, maxCell int) (string, error) {
	s.maybeCleanup()
	inline, err := inlineText(res, f, maxCell)
	if err != nil {
		return "", err
	}
	if len(inline) <= limit {
		return inline, nil
	}
	return s.spill(tool, conn, res, f, len(inline), limit)
}

// spill writes res to a file and returns its summary within limit bytes,
// which says res takes size bytes inline. If the file cannot be written, it
// returns as many leading rows as fit in limit, with a note saying why.
func (s *Store) spill(tool, conn string, res *result.Result, f render.Format, size, limit int) (string, error) {
	path, err := s.write(tool, conn, res)
	if err != nil {
		return capped(res, f, err, limit)
	}
	return summary(res, path, size, limit)
}

// inlineText renders res as it is returned inline: its cells cut to maxCell
// characters, 0 keeping them whole, then the footer.
func inlineText(res *result.Result, f render.Format, maxCell int) (string, error) {
	body, err := render.Render(res, f, maxCell)
	if err != nil {
		return "", err
	}
	return body + footer(res), nil
}

// Cleanup removes the regular files in Dir that are named like spill files
// and older than TTL, whichever Store wrote them. It leaves other files,
// directories and symlinks alone.
func (s *Store) Cleanup() error {
	if s.TTL <= 0 || s.Dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-s.TTL)
	var errs []error
	for _, e := range entries {
		if err := removeIfStale(s.Dir, e, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeIfStale removes e if it is a spill file last modified before cutoff.
func removeIfStale(dir string, e fs.DirEntry, cutoff time.Time) error {
	if !e.Type().IsRegular() || !spillName.MatchString(e.Name()) {
		return nil
	}
	info, err := e.Info()
	if err == nil && info.ModTime().Before(cutoff) {
		err = os.Remove(filepath.Join(dir, e.Name()))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed by a concurrent Cleanup
	}
	return err
}

func (s *Store) maybeCleanup() {
	now := s.now()
	s.mu.Lock()
	due := now.Sub(s.lastCleanup) >= cleanupInterval
	if due {
		s.lastCleanup = now
	}
	s.mu.Unlock()
	if due {
		_ = s.Cleanup() // best effort: the next run retries
	}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// write saves the full result as CSV in a new file under Dir and returns its
// absolute path.
func (s *Store) write(tool, conn string, res *result.Result) (string, error) {
	if s.Dir == "" {
		return "", errors.New("no spill directory configured")
	}
	data, err := render.Render(res, render.CSV, 0)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stamp := s.now().UTC().Format(stampLayout)
	file, err := os.CreateTemp(dir, "spill-"+safeName(tool)+"-"+safeName(conn)+"-"+stamp+"-*.csv")
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(file.Name()) //nolint:gosec // the file this call just created
		return "", err
	}
	return file.Name(), nil
}

// safeName keeps ASCII letters, digits, '-' and '_' of s, replaces anything
// else with '_', and cuts the result to maxNamePart bytes.
func safeName(s string) string {
	name := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return '_'
	}, s)
	if len(name) > maxNamePart {
		name = name[:maxNamePart]
	}
	return name
}

// capped renders as many leading rows of res as fit in limit bytes, for when
// the spill file could not be written.
func capped(res *result.Result, f render.Format, cause error, limit int) (string, error) {
	total := len(res.Rows)
	budget := limit - len(cappedNote(total, res, cause))
	body, shown := "", 0
	// Binary search for the most rows whose rendering fits in budget.
	for lo, hi := 0, total; lo <= hi; {
		mid := (lo + hi) / 2
		out, err := render.Render(head(res, mid), f, inlineMaxCell)
		if err != nil {
			return "", err
		}
		if len(out) <= budget {
			body, shown, lo = out, mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return body + cappedNote(shown, res, cause), nil
}

func cappedNote(shown int, res *result.Result, cause error) string {
	note := fmt.Sprintf("(%d of %s shown; could not write the spill file: %v", shown, count(len(res.Rows), "row"), cause)
	return note + truncation(res) + ")"
}

// footer counts the rows and says when res was truncated.
func footer(res *result.Result) string {
	return "(" + count(len(res.Rows), "row") + truncation(res) + ")"
}

func truncation(res *result.Result) string {
	switch {
	case !res.Truncated:
		return ""
	case res.TruncatedReason == "":
		return "; truncated"
	default:
		return "; truncated: " + res.TruncatedReason
	}
}

// head returns res with only its first n rows.
func head(res *result.Result, n int) *result.Result {
	return &result.Result{Columns: res.Columns, Rows: res.Rows[:n]}
}

// count returns "1 row", "2 rows" and so on.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
