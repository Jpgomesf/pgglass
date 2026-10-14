package tools_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

var spillFileLine = regexp.MustCompile(`(?m)^Full result: (.+)$`)

// The spill directory comes from HOME or XDG_CACHE_HOME, whose bytes need not
// be UTF-8, and JSON carries each byte that is not UTF-8 as U+FFFD, three
// bytes: the path the client gets must name the spill file, and the note of a
// failed write, which quotes the directory, must keep the result within the
// inline limit as the client decodes it.
func TestSpillDirectoryThatIsNotUTF8(t *testing.T) {
	t.Parallel()
	conns := map[string]config.Connection{}
	for i := range 400 {
		conns[fmt.Sprintf("connection-%03d", i)] = direct(unusedDSN)
	}
	for _, name := range []string{"caf\xe9", strings.Repeat("\xe9", 9000)} {
		s := connectSpillingTo(t, newConfig(config.RoleCheckWarn, conns), filepath.Join(t.TempDir(), name, "pgglass"))

		got := s.ok(t, "pg_list_connections", map[string]any{})
		if len(got) > config.InlineMaxBytes {
			t.Errorf("directory component of %d bytes: %d bytes at the client, %d of them U+FFFD, limit %d:\n%.200q\n...\n%.200q",
				len(name), len(got), 3*strings.Count(got, "\uFFFD"), config.InlineMaxBytes, got, got[max(0, len(got)-200):])
		}
		for _, m := range spillFileLine.FindAllStringSubmatch(got, -1) {
			if _, err := os.Stat(m[1]); err != nil {
				t.Errorf("directory component of %d bytes: the client got a spill path that names no file: %v", len(name), err)
			}
		}
	}
}
