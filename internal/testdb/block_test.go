package testdb

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	lockAuditLog = "LOCK TABLE app.audit_log IN ACCESS EXCLUSIVE MODE"
	readAuditLog = "SELECT count(*) FROM app.audit_log"
)

// Block returns the pids of the holder and the waiter once the waiter waits
// for the holder's lock.
func TestBlockMakesTheWaiterWaitForTheLock(t *testing.T) {
	holder, waiter := Conn(t, Superuser, DBApp), Conn(t, RoleClean, DBApp)
	wantHolder := Value[int](t, holder, "SELECT pg_backend_pid()")
	wantWaiter := Value[int](t, waiter, "SELECT pg_backend_pid()")

	holderPID, waiterPID := Block(t, holder, waiter, lockAuditLog, readAuditLog)
	if holderPID != wantHolder || waiterPID != wantWaiter {
		t.Errorf("Block = %d, %d; want the pids %d and %d", holderPID, waiterPID, wantHolder, wantWaiter)
	}
	observer := Conn(t, Superuser, DBApp)
	if !Value[bool](t, observer, "SELECT $1 = ANY (pg_blocking_pids($2))", holderPID, waiterPID) {
		t.Errorf("pid %d does not wait for pid %d", waiterPID, holderPID)
	}
	if wait := Value[string](t, observer, "SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1", waiterPID); wait != "Lock" {
		t.Errorf("the waiter's wait_event_type = %q, want Lock", wait)
	}
}

// A lock that cannot be taken fails the test and leaves the holder usable.
func TestBlockFailsWhenTheLockFails(t *testing.T) {
	holder, waiter := Conn(t, Superuser, DBApp), Conn(t, RoleClean, DBApp)
	r := &recorder{TB: t}
	attempt(func() { Block(r, holder, waiter, "LOCK TABLE app.nope", readAuditLog) })
	wantFatal(t, r, `testdb: LOCK TABLE app.nope: ERROR: relation "app.nope" does not exist`)
	if n := Value[int](t, holder, "SELECT 1"); n != 1 {
		t.Errorf("SELECT 1 on the holder = %d", n)
	}
}

// A role that may not call pg_blocking_pids cannot see the waiter wait.
func TestBlockFailsWhenItCannotSeeTheWaiterWait(t *testing.T) {
	const name = "testdb_hidden_locks"
	CreateDatabase(t, name, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_blocking_pids(integer) FROM PUBLIC")
	holder, waiter := Conn(t, RoleClean, name), Conn(t, RoleClean, name)
	r := &recorder{TB: t}
	const lock = "SELECT pg_advisory_xact_lock(1)"
	attempt(func() { Block(r, holder, waiter, lock, lock) })
	wantFatal(t, r, "testdb: wait for pid ")
	if !strings.Contains(r.msg, "permission denied for function pg_blocking_pids") {
		t.Errorf("message %q does not carry the server's reason", r.msg)
	}
}

// A waiter that never waits fails the test once blockTimeout has passed.
func TestBlockFailsWhenTheWaiterDoesNotWait(t *testing.T) {
	holder, waiter := Conn(t, Superuser, DBApp), Conn(t, RoleClean, DBApp)
	holderPID := Value[int](t, holder, "SELECT pg_backend_pid()")
	waiterPID := Value[int](t, waiter, "SELECT pg_backend_pid()")
	r := &recorder{TB: t}
	start := time.Now()
	attempt(func() { Block(r, holder, waiter, "SELECT pg_advisory_xact_lock(2)", "SELECT 1") })
	wantFatal(t, r, fmt.Sprintf("testdb: pid %d did not block on pid %d within %s", waiterPID, holderPID, blockTimeout))
	if elapsed := time.Since(start); elapsed < blockTimeout {
		t.Errorf("gave up after %v, before %v", elapsed, blockTimeout)
	}
}

// waitSQL must succeed once the lock is released: a failure fails the test.
func TestBlockReportsAWaiterThatFailsOnceUnblocked(t *testing.T) {
	holder, waiter := Conn(t, Superuser, DBApp), Conn(t, RoleClean, DBApp)
	const failing = "SELECT count(*) / 0 FROM app.audit_log"
	r := &recorder{}
	t.Run("block", func(t *testing.T) {
		r.TB = t
		Block(r, holder, waiter, lockAuditLog, failing)
	})
	if r.failed {
		t.Fatalf("Block failed: %s", r.msg)
	}
	if len(r.errors) != 1 || !strings.HasPrefix(r.errors[0], "testdb: "+failing+": ERROR: division by zero") {
		t.Errorf("errors = %q, want the waiter's failure", r.errors)
	}
}
