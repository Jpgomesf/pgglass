package guard

import "strings"

// CheckSearchPath returns a *Violation if schema, the one schema a query's
// search_path is set to, is in deniedSchemas. Check denies calls into those
// schemas by their qualified names only; with one on the search path, its
// functions could be called by unqualified names, which Check cannot tell
// apart from others. Callers must check every search_path they set for SQL
// that Check accepted.
func CheckSearchPath(schema string) error {
	// The engine quotes the schema with pgx, which drops NUL bytes:
	// "n\x00et" would name net.
	if strings.ContainsRune(schema, 0) {
		return violation("schema contains a NUL byte")
	}
	if deniedSchemas[strings.ToLower(schema)] {
		return violation("schema %s is not allowed as the search_path: its functions are denied", schema)
	}
	return nil
}
