package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/render"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

// maxFanout is the most connections one pg_query call may fan out to.
const maxFanout = 10

// fanoutColumn starts every row of a fanned-out result: the connection
// the row came from.
var fanoutColumn = result.Column{Name: "connection", TypeOID: pgtype.TextOID, TypeName: "text"}

// fanoutResult is what the statement of a fanned-out call did on one
// connection.
type fanoutResult struct {
	conn   string
	res    *result.Result // nil when err is set
	notice string         // the role check's, as a single call starts with
	err    error
}

// fanout runs pg_query on each of args.Connections at once, the way a single
// call runs on one: the guard once, then on each connection the role check
// and the engine, with its limits. It writes one audit line per connection.
// The rows come back under a first column, connection, unless the columns
// differ; each connection that failed adds a line, and the call fails only
// when every connection did.
func (h *Handlers) fanout(ctx context.Context, args queryArgs, c *call) (string, error) {
	if err := h.checkFanout(args); err != nil {
		return "", err
	}
	c.audited = true
	format, err := h.checkQuery(args)
	if err != nil {
		for _, conn := range args.Connections {
			h.audit(c.on(conn), err)
		}
		return "", err
	}
	results := h.queryEach(ctx, args, c)
	c.notice = fanoutNotices(results)
	return h.fanoutText(results, format, h.room(c))
}

// checkFanout checks that args name, in place of connection, 1 to maxFanout
// configured connections, each once.
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

// queryEach runs the statement of args on every connection at once and
// audits each run.
func (h *Handlers) queryEach(ctx context.Context, args queryArgs, c *call) []fanoutResult {
	results := make([]fanoutResult, len(args.Connections))
	var wg sync.WaitGroup
	for i, conn := range args.Connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cc := c.on(conn)
			res, err := h.queryOn(ctx, conn, args, cc)
			h.audit(cc, err)
			results[i] = fanoutResult{conn: conn, res: res, notice: cc.notice, err: err}
		}()
	}
	wg.Wait()
	return results
}

// on returns a copy of c for its run on the connection conn.
func (c *call) on(conn string) *call {
	cc := *c
	cc.connection = conn
	return &cc
}

// fanoutNotices joins the role check notices of results, each after the
// name of its connection.
func fanoutNotices(results []fanoutResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.notice != "" {
			b.WriteString(r.conn + ": " + r.notice)
		}
	}
	return b.String()
}

// fanoutText renders results within room bytes: the rows of the connections
// that succeeded, then a line for each that failed, its error as a single
// call reports it. The lines share half of room, each cut to its share, and
// the rows take the rest. When none succeeded, it fails with the lines, which
// then share all of room.
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
	return text + lines, nil
}

// failureLines renders a line for each of failed: its connection, then its
// error as a single call reports it, cut in the middle to an equal share of
// room.
func failureLines(failed []fanoutResult, room int) string {
	lines := make([]string, len(failed))
	for i, r := range failed {
		line := r.conn + ": " + strings.ReplaceAll(errorText(r.err), "\n", "\n  ")
		lines[i] = cutMiddle(line, room/len(failed)-1) // less its line break
	}
	return strings.Join(lines, "\n")
}

// fanoutRows renders ok, results that all succeeded, within room bytes:
// merged when their columns match, otherwise one section per connection.
// Either way, what does not fit is spilled to a file. A section names its
// file even past its share, so when the sections take more than room, as
// long connection names and a long spill directory path can make them, they
// go to one more file, a line each, and fanoutRows returns its summary.
func (h *Handlers) fanoutRows(ok []fanoutResult, format render.Format, room int) (string, error) {
	names := make([]string, len(ok))
	for i, r := range ok {
		names[i] = r.conn
	}
	if sameFanoutColumns(ok) {
		return h.spill.OutputWithin("pg_query", strings.Join(names, "_"), mergeFanout(ok), format, room)
	}
	text, err := h.fanoutSections(ok, format, room)
	if err != nil || len(text) <= room {
		return text, err
	}
	return h.linesOutput("pg_query", strings.Join(names, "_"), "line", strings.Split(text, "\n"), "", room)
}

// fanoutSections renders ok, results whose columns differ, as a section per
// connection under its name, after a line that says why. The sections share
// room with their names and that line: a section past its share is spilled
// to a file of its own, which it names even when its share is too small for
// that.
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

// sameFanoutColumns reports whether the results in ok have the same
// columns, by name and type name: an enum or another user-defined type has a
// different OID in each database.
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

// mergeFanout returns the rows of ok, results with the same columns, in
// their order, each after the name of its connection. The merged result is
// truncated when any of ok is, and says which.
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
