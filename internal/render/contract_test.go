package render_test

import (
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// The API other packages build against. Changing it must fail here first.
var _ func(*result.Result, render.Format, int) (string, error) = render.Render

// Integration converts the validated config string with render.Format(cfg.Defaults.Format).
func TestFormatsMatchConfig(t *testing.T) {
	pairs := map[render.Format]string{
		render.TSV:      config.FormatTSV,
		render.JSON:     config.FormatJSON,
		render.Markdown: config.FormatMarkdown,
	}
	for f, s := range pairs {
		if string(f) != s {
			t.Errorf("render.Format %q != config value %q", f, s)
		}
	}
	if render.CSV != "csv" {
		t.Errorf("render.CSV = %q", render.CSV)
	}
}
