package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/render"
	"github.com/jpgomesf/pgglass/internal/result"
	"github.com/jpgomesf/pgglass/internal/spill"
)

const maxFanout = 10

var fanoutColumn = result.Column{Name: "connection", TypeOID: pgtype.TextOID, TypeName: "text"}

type fanoutResult struct {
	conn       string
	searchPath string
	res        *result.Result
	notice     string
	err        error
}

func (h *Handlers) fanout(ctx context.Context, args queryArgs, c *call) (string, error) {
	if err := h.checkFanout(args); err != nil {
		return "", err
	}
	c.audited = true
	format, err := h.checkQuery(args)
	results, ready := h.searchPaths(args)
	if err == nil {
		err = h.confirm(c, "pg_query", ready, args.SQL, args.Params)
	}
	if err != nil {
		if !asksUser(err) {
			for _, conn := range args.Connections {
				h.audit(c.on(conn), err)
			}
		}
		return "", err
	}
	h.queryEach(ctx, args, results, c)
	c.notice = fanoutNotices(results)
	return h.fanoutText(results, format, h.room(c))
}

func (h *Handlers) searchPaths(args queryArgs) ([]fanoutResult, []target) {
	results := make([]fanoutResult, len(args.Connections))
	var ready []target
	for i, conn := range args.Connections {
		searchPath, err := h.searchPath(conn, args.Schema)
		results[i] = fanoutResult{conn: conn, searchPath: searchPath, err: err}
		if err == nil {
			ready = append(ready, target{conn: conn, searchPath: searchPath})
		}
	}
	return results, ready
}

func (h *Handlers) checkFanout(args queryArgs) error {
	if args.Connection != "" {
		return errors.New("pass connection or connections, not both")
	}
	if n := len(args.Connections); n == 0 || n > maxFanout {
		return fmt.Errorf("connections must list 1 to %d names, got %d", maxFanout, n)
	}
	for i, name := range args.Connections {
		if _, ok := h.cfg.Connections[name]; !ok {
			return engine.UnknownConnection(name, h.cfg.Connections)
		}
		if slices.Contains(args.Connections[:i], name) {
			return fmt.Errorf("connections lists %q twice", name)
		}
	}
	return nil
}

func (h *Handlers) queryEach(ctx context.Context, args queryArgs, results []fanoutResult, c *call) {
	var wg sync.WaitGroup
	for i := range results {
		r := &results[i]
		wg.Go(func() {
			cc := c.on(r.conn)
			if r.err == nil {
				r.res, r.err = h.queryOn(ctx, target{conn: r.conn, searchPath: r.searchPath}, args, cc)
			}
			h.audit(cc, r.err)
			r.notice = cc.notice
		})
	}
	wg.Wait()
}

func (c *call) on(conn string) *call {
	cc := *c
	cc.connection = conn
	return &cc
}

func fanoutNotices(results []fanoutResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.notice != "" {
			b.WriteString(r.conn + ": " + r.notice)
		}
	}
	return b.String()
}

func (h *Handlers) fanoutText(results []fanoutResult, format render.Format, room int) (string, error) {
	var ok, failed []fanoutResult
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, r)
		} else {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return "", errors.New(failureLines(failed, room))
	}
	if len(failed) == 0 {
		return h.fanoutRows(ok, format, room)
	}
	lines := "\n\n" + failureLines(failed, room/2)
	text, err := h.fanoutRows(ok, format, room-len(lines))
	if err != nil {
		return "", err
	}
	return text + spill.CutMiddle(lines, room-len(text)), nil
}

func failureLines(failed []fanoutResult, room int) string {
	lines := make([]string, len(failed))
	for i, r := range failed {
		line := r.conn + ": " + strings.ReplaceAll(errorText(r.err), "\n", "\n  ")
		lines[i] = spill.CutMiddle(line, room/len(failed)-1)
	}
	return strings.Join(lines, "\n")
}

func (h *Handlers) fanoutRows(ok []fanoutResult, format render.Format, room int) (string, error) {
	names := make([]string, len(ok))
	for i, r := range ok {
		names[i] = r.conn
	}
	if sameFanoutColumns(ok) {
		return h.spill.OutputWithin("pg_query", strings.Join(names, "_"), mergeFanout(ok), format, room)
	}
	text, err := h.fanoutSections(ok, format, room)
	if err == nil && len(text) > room {
		text, err = h.fanoutSections(ok, format, 0)
	}
	if err != nil || len(text) <= room {
		return text, err
	}
	return h.linesOutput("pg_query", strings.Join(names, "_"), "line", strings.Split(text, "\n"), "", room)
}

func (h *Handlers) fanoutSections(ok []fanoutResult, format render.Format, room int) (string, error) {
	sections := []string{"Columns differ across connections, so each has its own result."}
	room -= len(sections[0])
	for _, r := range ok {
		room -= len("\n\n" + r.conn + ":\n")
	}
	for _, r := range ok {
		text, err := h.spill.OutputWithin("pg_query", r.conn, r.res, format, room/len(ok))
		if err != nil {
			return "", err
		}
		sections = append(sections, r.conn+":\n"+text)
	}
	return strings.Join(sections, "\n\n"), nil
}

func sameFanoutColumns(ok []fanoutResult) bool {
	first := ok[0].res.Columns
	for _, r := range ok[1:] {
		if !slices.EqualFunc(first, r.res.Columns, func(a, b result.Column) bool {
			return a.Name == b.Name && a.TypeName == b.TypeName
		}) {
			return false
		}
	}
	return true
}

func mergeFanout(ok []fanoutResult) *result.Result {
	merged := &result.Result{Columns: append([]result.Column{fanoutColumn}, ok[0].res.Columns...)}
	var cut []string
	for _, r := range ok {
		conn := r.conn
		for _, row := range r.res.Rows {
			merged.Rows = append(merged.Rows, append([]*string{&conn}, row...))
		}
		if r.res.Truncated {
			cut = append(cut, conn+" "+r.res.TruncatedReason)
		}
	}
	merged.Truncated, merged.TruncatedReason = len(cut) > 0, strings.Join(cut, ", ")
	return merged
}
