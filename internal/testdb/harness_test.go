package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

// recorder is a testing.TB that records Skip and Fatal instead of acting on them.
type recorder struct {
	testing.TB
	skipped, failed bool
	msg             string
}

func (r *recorder) Helper() {}

func (r *recorder) Context() context.Context { return context.Background() }

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

// record runs fn with a recorder on its own goroutine, so Skip and Fatal can stop it.
func record(fn func(testing.TB)) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r
}

func TestGetFailsWhenDockerIsUnavailable(t *testing.T) {
	h := &harness{managed: true, start: func(context.Context) (*server, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}}
	r := record(func(tb testing.TB) { h.get(tb) })

	if testing.Short() {
		if !r.skipped || r.failed {
			t.Errorf("under -short: skipped=%v failed=%v, want a skip", r.skipped, r.failed)
		}
		return
	}
	if !r.failed || r.skipped {
		t.Fatalf("skipped=%v failed=%v, want a failure, never a skip", r.skipped, r.failed)
	}
	if !strings.Contains(r.msg, "Cannot connect to the Docker daemon") {
		t.Errorf("message %q does not carry the Docker error", r.msg)
	}
}

func TestGetRequiresMain(t *testing.T) {
	h := &harness{start: func(context.Context) (*server, error) {
		t.Error("start called without Main")
		return nil, nil
	}}
	r := record(func(tb testing.TB) { h.get(tb) })

	if testing.Short() {
		if !r.skipped {
			t.Error("under -short: want a skip")
		}
		return
	}
	if !r.failed || !strings.Contains(r.msg, "testdb.Main") {
		t.Errorf("failed=%v msg=%q, want a failure pointing at testdb.Main", r.failed, r.msg)
	}
}

func TestGetStartsTheServerOnce(t *testing.T) {
	calls := 0
	srv := &server{host: "localhost", port: "5432"}
	h := &harness{managed: true, start: func(context.Context) (*server, error) {
		calls++
		return srv, nil
	}}

	for range 3 {
		r := record(func(tb testing.TB) {
			if got := h.get(tb); got != srv {
				t.Errorf("get returned %p, want %p", got, srv)
			}
		})
		if testing.Short() != r.skipped || r.failed {
			t.Fatalf("skipped=%v failed=%v (short=%v)", r.skipped, r.failed, testing.Short())
		}
	}
	want := 1
	if testing.Short() {
		want = 0
	}
	if calls != want {
		t.Errorf("start called %d times, want %d", calls, want)
	}
}

func TestTerminateWithoutServerIsNoop(t *testing.T) {
	if err := (&harness{}).terminate(); err != nil {
		t.Errorf("terminate: %v", err)
	}
}

func TestDSN(t *testing.T) {
	srv := &server{host: "localhost", port: "55432"}
	for _, tc := range []struct{ role, database string }{{RoleClean, DBApp}, {"odd role", "my db"}} {
		u, err := url.Parse(srv.dsn(tc.role, tc.database))
		if err != nil {
			t.Fatalf("dsn(%q, %q) does not parse: %v", tc.role, tc.database, err)
		}
		password, _ := u.User.Password()
		if u.Scheme != "postgres" || u.User.Username() != tc.role || password != tc.role ||
			u.Host != "localhost:55432" || u.Path != "/"+tc.database || u.Query().Get("sslmode") != "disable" {
			t.Errorf("dsn(%q, %q) = %s", tc.role, tc.database, u.Redacted())
		}
	}
}

func TestImage(t *testing.T) {
	t.Setenv("MCP_PG_IMAGE", "")
	if got := image(); got != DefaultImage {
		t.Errorf("image() = %q, want %q", got, DefaultImage)
	}
	t.Setenv("MCP_PG_IMAGE", "postgres:15-alpine")
	if got := image(); got != "postgres:15-alpine" {
		t.Errorf("image() = %q, want postgres:15-alpine", got)
	}
}
