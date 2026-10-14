package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/preflight"
)

type roleChecks struct {
	mode  string
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

type roleCheck struct {
	running chan struct{}

	mu     sync.Mutex
	report *preflight.Report
	err    error
	shown  bool
}

type blockedError struct {
	conn   string
	report preflight.Report
}

func (e *blockedError) Error() string {
	return fmt.Sprintf("connection %q refused (role_check strict): its role can do more than read.\n%s", e.conn, e.report)
}

const roleCheckFailed = "role check failed, and role_check strict refuses connections it cannot check: "

type roleCheckError struct{ err error }

func (e *roleCheckError) Error() string { return roleCheckFailed + e.err.Error() }

func (e *roleCheckError) Unwrap() error { return e.err }

func (rc *roleChecks) gate(ctx context.Context, conn string) (notice string, err error) {
	return rc.pass(ctx, conn, true)
}

func (rc *roleChecks) admit(ctx context.Context, conn string) error {
	_, err := rc.pass(ctx, conn, false)
	return err
}

func (rc *roleChecks) pass(ctx context.Context, conn string, show bool) (notice string, err error) {
	if rc.mode == config.RoleCheckOff {
		return "", nil
	}
	report, fresh, err := rc.check(conn).run(ctx, show, func(ctx context.Context) (preflight.Report, error) {
		return rc.audit(ctx, conn)
	})
	switch {
	case err != nil && rc.mode == config.RoleCheckStrict:
		return "", &dbError{&roleCheckError{err}}
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

func (c *roleCheck) run(ctx context.Context, show bool, audit func(context.Context) (preflight.Report, error)) (report preflight.Report, fresh bool, err error) {
	select {
	case c.running <- struct{}{}:
	case <-ctx.Done():
		return preflight.Report{}, false, ctx.Err()
	}
	defer func() { <-c.running }()
	if r, _ := c.state(); r == nil {
		if err = c.record(audit(ctx)); err != nil {
			return preflight.Report{}, false, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh = show && !c.shown
	c.shown = c.shown || show
	return *c.report, fresh, nil
}

func (c *roleCheck) record(report preflight.Report, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.report = &report
	}
	c.err = err
	return err
}

func (c *roleCheck) state() (*preflight.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report, c.err
}
