package spill_test

import (
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
	"github.com/jpgomesf/mcp-postgres/internal/spill"
)

// The API other packages build against. Changing it must fail here first.
var (
	_ func(*spill.Store, string, string, *result.Result, render.Format) (string, error) = (*spill.Store).Output
	_ func(*spill.Store) error                                                          = (*spill.Store).Cleanup

	_ = spill.Store{Dir: "/tmp/mcp-postgres", TTL: time.Hour, InlineMaxBytes: 10_000, Now: time.Now}
)
