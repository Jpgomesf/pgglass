package testdb

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { Main(m) }

type recorder struct {
	testing.TB
	skipped, failed bool
	msg             string
	errors          []string
}

func (r *recorder) Helper() {}

func (r *recorder) Skip(args ...any) {
	r.skipped, r.msg = true, fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recorder) Fatal(args ...any) {
	r.failed, r.msg = true, fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed, r.msg = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func attempt(f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	<-done
}

func wantFatal(t *testing.T, r *recorder, prefix string) {
	t.Helper()
	if !r.failed || r.skipped || !strings.HasPrefix(r.msg, prefix) {
		t.Errorf("failed=%v skipped=%v message %q; want a failure starting %q", r.failed, r.skipped, r.msg, prefix)
	}
}

// Without Docker, an integration test fails rather than skips, so a run
// without Docker cannot pass by testing nothing.
func TestGetFailsWhenDockerIsUnavailable(t *testing.T) {
	h := &harness{managed: true, start: func(context.Context) (*server, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}}
	r := &recorder{}
	attempt(func() { h.get(r) })

	if testing.Short() {
		if !r.skipped || r.failed {
			t.Errorf("under -short: skipped=%v failed=%v, want a skip", r.skipped, r.failed)
		}
		return
	}
	wantFatal(t, r, "testdb: start Postgres container (is Docker running?): Cannot connect to the Docker daemon")
}

// A package whose TestMain does not call Main would leave its container
// running, so its first integration test fails before starting one.
func TestGetFailsWithoutMain(t *testing.T) {
	started := false
	h := &harness{start: func(context.Context) (*server, error) {
		started = true
		return nil, nil
	}}
	r := &recorder{}
	attempt(func() { h.get(r) })

	if testing.Short() {
		if !r.skipped {
			t.Errorf("under -short: skipped=%v, want a skip", r.skipped)
		}
		return
	}
	wantFatal(t, r, "testdb: call testdb.Main(m) from TestMain so the container is terminated")
	if started {
		t.Error("the container was started")
	}
}

// A mistyped PGGLASS_PG_IMAGE fails the test with Docker's reason, and leaves
// no container behind.
func TestStartFailsOnAnImageDockerRefuses(t *testing.T) {
	t.Setenv("PGGLASS_PG_IMAGE", "pgglass/Not-A-Valid-Reference")
	h := &harness{managed: true, start: startServer}
	r := &recorder{TB: t}
	attempt(func() { h.get(r) })

	if testing.Short() {
		if !r.skipped {
			t.Errorf("under -short: skipped=%v, want a skip", r.skipped)
		}
		return
	}
	wantFatal(t, r, "testdb: start Postgres container (is Docker running?): ")
	if !strings.Contains(r.msg, "invalid reference format") {
		t.Errorf("message %q does not carry Docker's reason", r.msg)
	}
	if h.srv != nil {
		t.Errorf("a server came back: %+v", h.srv)
	}
}

func TestImageIsPGGLASS_PG_IMAGEOrTheDefault(t *testing.T) {
	t.Setenv("PGGLASS_PG_IMAGE", "postgres:16-alpine")
	if got := Image(); got != "postgres:16-alpine" {
		t.Errorf("Image() = %q, want postgres:16-alpine", got)
	}
	t.Setenv("PGGLASS_PG_IMAGE", "")
	if got := Image(); got != DefaultImage {
		t.Errorf("Image() without PGGLASS_PG_IMAGE = %q, want %s", got, DefaultImage)
	}
}

// exit terminates the container Main started and keeps the tests' exit code.
func TestExitTerminatesTheContainer(t *testing.T) {
	if code := (&harness{}).exit(3); code != 3 {
		t.Errorf("exit(3) without a container = %d, want 3", code)
	}

	h := &harness{managed: true, start: startServer}
	srv := h.get(t)
	t.Cleanup(func() { _ = h.terminate() })
	dsn := srv.dsn(RoleClean, DBApp)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect before exit: %v", err)
	}
	if n := Value[int](t, conn, "SELECT count(*) FROM app.customers"); n != 50 {
		t.Errorf("customers = %d, want the fixture's 50", n)
	}
	_ = conn.Close(t.Context())

	if code := h.exit(0); code != 0 {
		t.Errorf("exit(0) = %d, want 0", code)
	}
	if running := srv.ctr.IsRunning(); running {
		t.Error("the container still runs after exit")
	}
	if conn, err := pgx.Connect(t.Context(), dsn); err == nil {
		_ = conn.Close(t.Context())
		t.Error("the server still answers after exit")
	}
}

func TestExecReportsADatabaseItCannotReach(t *testing.T) {
	err := shared.get(t).exec(t.Context(), "testdb_absent", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), `database "testdb_absent" does not exist`) {
		t.Errorf("exec in a missing database = %v, want the server's refusal", err)
	}
}
