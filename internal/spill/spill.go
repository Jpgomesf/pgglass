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
	"unicode/utf8"

	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
)

const (
	inlineMaxCell   = 200
	cleanupInterval = time.Hour
	stampLayout     = "20060102T150405Z"
	maxNamePart     = 32
)

var spillName = regexp.MustCompile(`^spill-[A-Za-z0-9_-]*-[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9]+\.csv$`)

// Store writes oversized outputs under Dir and removes them after TTL. It is
// safe for concurrent use and must not be copied after first use.
type Store struct {
	Dir            string
	TTL            time.Duration
	InlineMaxBytes int
	Now            func() time.Time

	mu          sync.Mutex
	lastCleanup time.Time
}

// OutputWithin renders res in format f within limit bytes: inline, cells cut to
// 200 characters, or else as a summary of a CSV spill file holding all of it.
func (s *Store) OutputWithin(tool, conn string, res *result.Result, f render.Format, limit int) (string, error) {
	return s.output(tool, conn, res, f, limit, inlineMaxCell)
}

// OutputWhole is OutputWithin without cutting inline cells, for results whose
// every value counts: one that fits only with cells cut goes to the file.
func (s *Store) OutputWhole(tool, conn string, res *result.Result, f render.Format, limit int) (string, error) {
	return s.output(tool, conn, res, f, limit, 0)
}

func (s *Store) output(tool, conn string, res *result.Result, f render.Format, limit, maxCell int) (string, error) {
	s.maybeCleanup()
	inline, err := inlineText(res, f, maxCell)
	if err != nil {
		return "", err
	}
	if len(inline) <= limit {
		return inline, nil
	}
	path, err := s.write(tool, conn, res)
	if err != nil {
		return capped(res, f, err, limit, maxCell)
	}
	return summary(res, path, len(inline), limit)
}

func inlineText(res *result.Result, f render.Format, maxCell int) (string, error) {
	body, err := render.Render(res, f, maxCell)
	if err != nil {
		return "", err
	}
	return body + footer(res), nil
}

// Cleanup removes the regular files in Dir named like spill files and older
// than TTL, whichever Store wrote them, and leaves everything else alone.
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

func removeIfStale(dir string, e fs.DirEntry, cutoff time.Time) error {
	if !e.Type().IsRegular() || !spillName.MatchString(e.Name()) {
		return nil
	}
	info, err := e.Info()
	if err == nil && info.ModTime().Before(cutoff) {
		err = os.Remove(filepath.Join(dir, e.Name()))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
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
		_ = s.Cleanup()
	}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

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
	if !utf8.ValidString(dir) {
		return "", errors.New("the spill directory's path is not UTF-8, so the client could not open the file")
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

func capped(res *result.Result, f render.Format, cause error, limit, maxCell int) (string, error) {
	total := len(res.Rows)
	why := fit(cause.Error(), limit/2-len(cappedNote(total, res, "")))
	budget := limit - len(cappedNote(total, res, why))
	body, shown := "", 0
	for lo, hi := 0, total; lo <= hi; {
		mid := (lo + hi) / 2
		out, err := render.Render(head(res, mid), f, maxCell)
		if err != nil {
			return "", err
		}
		if len(out) <= budget {
			body, shown, lo = out, mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return body + cappedNote(shown, res, why), nil
}

func cappedNote(shown int, res *result.Result, why string) string {
	return fmt.Sprintf("(%d of %s shown; could not write the spill file: %s", shown, count(len(res.Rows), "row"), why) + truncation(res) + ")"
}

// CutMiddle returns s if it fits in limit bytes, or else its start and its end
// around a note of how many bytes it cut, "…[1234 bytes cut]…", within limit
// when that holds the note. It cuts between UTF-8 characters.
func CutMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	keep := max(limit-len(cutNote(len(s))), 0)
	end := keep / 2
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	start := len(s) - (keep - keep/2)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:end] + cutNote(start-end) + s[start:]
}

func cutNote(n int) string {
	return "…[" + strconv.Itoa(n) + " bytes cut]…"
}

func fit(s string, limit int) string {
	if cut := CutMiddle(s, limit); len(cut) <= limit {
		return cut
	}
	return s
}

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

func head(res *result.Result, n int) *result.Result {
	return &result.Result{Columns: res.Columns, Rows: res.Rows[:n]}
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
