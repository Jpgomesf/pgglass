package tools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

func TestAdmitLeavesTheFindingsToTheNextResult(t *testing.T) {
	for _, mode := range []string{config.RoleCheckWarn, config.RoleCheckStrict} {
		rc, calls := fakeChecks(mode, report(warnReport))
		if err := rc.admit(t.Context(), "reader"); err != nil {
			t.Errorf("%s: admit = %v, want nil", mode, err)
		}
		if notice, err := rc.gate(t.Context(), "reader"); err != nil || notice != "role check: "+warnReport.String()+"\n\n" {
			t.Errorf("%s: gate after admit = %q, %v; want the findings", mode, notice, err)
		}
		if err := rc.admit(t.Context(), "reader"); err != nil {
			t.Errorf("%s: second admit = %v, want nil", mode, err)
		}
		if notice, err := rc.gate(t.Context(), "reader"); err != nil || notice != "" {
			t.Errorf("%s: second gate = %q, %v; want no notice", mode, notice, err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("%s: audited %d times, want once", mode, n)
		}
	}
}

func TestAdmitRefusesWhatGateRefuses(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckStrict, report(blockReport))
	for range 2 {
		var blocked *blockedError
		if err := rc.admit(t.Context(), "su"); !errors.As(err, &blocked) {
			t.Errorf("strict admit of a critical finding = %v, want a *blockedError", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("audited %d times, want once", n)
	}

	boom := errors.New("connection refused")
	rc, _ = fakeChecks(config.RoleCheckStrict, failure(boom))
	var dbErr *dbError
	if err := rc.admit(t.Context(), "db"); !errors.As(err, &dbErr) || !errors.Is(err, boom) {
		t.Errorf("strict admit after a failed audit = %v, want a *dbError wrapping it", err)
	}
	rc, _ = fakeChecks(config.RoleCheckWarn, failure(boom))
	if err := rc.admit(t.Context(), "db"); err != nil {
		t.Errorf("warn admit after a failed audit = %v, want nil", err)
	}

	rc, calls = fakeChecks(config.RoleCheckOff, report(blockReport))
	if err := rc.admit(t.Context(), "su"); err != nil || calls.Load() != 0 {
		t.Errorf("off admit = %v after %d audits, want nil after none", err, calls.Load())
	}
}

func TestAdmitAndGateShowTheFindingsOnceUnderConcurrency(t *testing.T) {
	rc, calls := fakeChecks(config.RoleCheckWarn, report(warnReport))
	var (
		wg      sync.WaitGroup
		notices atomic.Int32
	)
	for i := range 40 {
		wg.Go(func() {
			if i%2 == 0 {
				_ = rc.admit(context.Background(), "reader")
				return
			}
			if notice, _ := rc.gate(context.Background(), "reader"); notice != "" {
				notices.Add(1)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || notices.Load() != 1 {
		t.Errorf("%d audits and %d notices, want one of each", calls.Load(), notices.Load())
	}
}
