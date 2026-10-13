package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// blockTimeout bounds how long Block waits for the waiter to block.
const blockTimeout = 10 * time.Second

// Block makes waiter wait for a lock that holder keeps until t finishes, and
// returns their pids: holder runs lockSQL in a transaction it keeps open,
// then waiter runs waitSQL in the background, and Block returns once waiter
// waits for holder. When t finishes, holder rolls back, and waitSQL must then
// succeed.
func Block(t testing.TB, holder, waiter *pgx.Conn, lockSQL, waitSQL string) (holderPID, waiterPID int) {
	t.Helper()
	holderPID = Value[int](t, holder, "SELECT pg_backend_pid()")
	waiterPID = Value[int](t, waiter, "SELECT pg_backend_pid()")
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatalf("testdb: begin: %v", err)
	}
	if _, err := tx.Exec(t.Context(), lockSQL); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("testdb: %s: %v", lockSQL, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(context.Background(), waitSQL)
		done <- err
	}()
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		if err := <-done; err != nil {
			t.Errorf("testdb: %s: %v", waitSQL, err)
		}
	})
	for deadline := time.Now().Add(blockTimeout); ; time.Sleep(20 * time.Millisecond) {
		var blocked bool
		if err := tx.QueryRow(t.Context(), "SELECT $1 = ANY (pg_blocking_pids($2))", holderPID, waiterPID).Scan(&blocked); err != nil {
			t.Fatalf("testdb: wait for pid %d to block: %v", waiterPID, err)
		}
		if blocked {
			return holderPID, waiterPID
		}
		if time.Now().After(deadline) {
			t.Fatalf("testdb: pid %d did not block on pid %d within %s", waiterPID, holderPID, blockTimeout)
		}
	}
}
