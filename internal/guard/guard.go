// Package guard rejects SQL that is not a single read-only statement, using
// the Postgres parser (libpg_query via github.com/wasilibs/go-pgquery, pure
// Go) rather than keyword matching. It is defense in depth: the engine's READ
// ONLY transaction is the guarantee.
package guard

import "errors"

var errNotImplemented = errors.New("not implemented")

// Violation explains why SQL was rejected.
type Violation struct {
	Reason string
}

// Error returns the reason.
func (v *Violation) Error() string {
	return v.Reason
}

// Check returns nil if sql is exactly one statement that only reads, and a
// *Violation otherwise, including when sql does not parse.
func Check(sql string) error {
	return errNotImplemented
}
