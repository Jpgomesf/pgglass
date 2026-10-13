package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/preflight"
)

// roleChecks audits the role of each connection once, on first use, and
// applies role_check: strict refuses a role with a critical finding, a way
// out of the read-only transaction such as superuser or EXECUTE on dblink
// (see preflight.SeverityCritical), and shows warnings, such as table writes,
// once; warn shows the findings once; off skips the audit.
type roleChecks struct {
	mode  string // config.RoleCheck*
	audit func(ctx context.Context, conn string) (preflight.Report, error)

	mu    sync.Mutex
	conns map[string]*roleCheck
}

func newRoleChecks(mode string, eng *engine.Manager) *roleChecks {
	return &roleChecks{
		mode: mode,
		audit: func(ctx context.Context, conn string) (preflight.Report, error) {
			return preflight.Audit(ctx, eng.Querier(conn))
		},
		conns: make(map[string]*roleCheck),
	}
}

// roleCheck is the audit of one connection. A failed audit is retried by the
// next call; a report, once made, is kept.
type roleCheck struct {
	// running holds a token during an audit, so one runs at a time. A
	// channel, unlike a mutex, lets a waiting call give up when it ends.
	running chan struct{}

	mu     sync.Mutex
	report *preflight.Report // nil until an audit succeeds
	err    error             // why the last audit failed
}

// blockedError refuses a connection under role_check strict.
type blockedError struct {
	conn   string
	report preflight.Report
}

func (e *blockedError) Error() string {
	return fmt.Sprintf("connection %q refused (role_check strict): its role can do more than read.\n%s", e.conn, e.report)
}

// gate audits the role of conn on first use and decides whether a call may
// go on. The notice it returns starts the call's result: the findings, the
// first time they are known, or why the audit failed under role_check warn.
func (rc *roleChecks) gate(ctx context.Context, conn string) (notice string, err error) {
	if rc.mode == config.RoleCheckOff {
		return "", nil
	}
	report, fresh, err := rc.check(conn).run(ctx, func(ctx context.Context) (preflight.Report, error) {
		return rc.audit(ctx, conn)
	})
	switch {
	case err != nil && rc.mode == config.RoleCheckStrict:
		return "", &dbError{fmt.Errorf("role check failed, and role_check strict refuses connections it cannot check: %w", err)}
	case err != nil:
		return fmt.Sprintf("role check failed (role_check warn, so the call goes on): %v\n\n", err), nil
	case rc.mode == config.RoleCheckStrict && report.Blocking():
		return "", &blockedError{conn: conn, report: report}
	case fresh && len(report.Findings) > 0:
		return "role check: " + report.String() + "\n\n", nil
	default:
		return "", nil
	}
}

// status describes the role check of conn for pg_list_connections.
func (rc *roleChecks) status(conn string) string {
	if rc.mode == config.RoleCheckOff {
		return "role check off"
	}
	report, err := rc.check(conn).state()
	switch {
	case report != nil && rc.mode == config.RoleCheckStrict && report.Blocking():
		return report.String() + "\nrefused: role_check is strict"
	case report != nil:
		return report.String()
	case err != nil:
		return "role check failed: " + err.Error()
	default:
		return "role not checked yet: checked on first use"
	}
}

func (rc *roleChecks) check(conn string) *roleCheck {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	c, ok := rc.conns[conn]
	if !ok {
		c = &roleCheck{running: make(chan struct{}, 1)}
		rc.conns[conn] = c
	}
	return c
}

// run returns the report, auditing first unless an audit has succeeded;
// fresh reports whether this call made the report. A call that ends while
// another audits returns its context's error and records no failure.
func (c *roleCheck) run(ctx context.Context, audit func(context.Context) (preflight.Report, error)) (report preflight.Report, fresh bool, err error) {
	select {
	case c.running <- struct{}{}:
	case <-ctx.Done():
		return preflight.Report{}, false, ctx.Err()
	}
	defer func() { <-c.running }()
	if r, _ := c.state(); r != nil {
		return *r, false, nil
	}
	report, err = audit(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.err = err
		return preflight.Report{}, false, err
	}
	c.report, c.err = &report, nil
	return report, true, nil
}

func (c *roleCheck) state() (*preflight.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report, c.err
}
