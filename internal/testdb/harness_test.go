package testdb

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

// recorder is a testing.TB that records Skip and Fatalf instead of acting on
// them, so a test can tell which one the harness called.
type recorder struct {
	testing.TB
	skipped, failed bool
	msg             string
}

func (r *recorder) Helper() {}

func (r *recorder) Skip(args ...any) {
	r.skipped, r.msg = true, fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed, r.msg = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// Without Docker, an integration test fails rather than skips, so a run
// without Docker cannot pass by testing nothing.
func TestGetFailsWhenDockerIsUnavailable(t *testing.T) {
	h := &harness{managed: true, start: func(context.Context) (*server, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}}
	r := &recorder{}
	done := make(chan struct{})
	go func() { // Skip and Fatalf end this goroutine, not the test's
		defer close(done)
		h.get(r)
	}()
	<-done

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
