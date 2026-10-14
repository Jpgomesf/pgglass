package guard

import "strings"

// CheckSearchPath returns a *Violation if schema is in deniedSchemas, since
// Check misses unqualified calls into it; check every search_path set.
func CheckSearchPath(schema string) error {
	if strings.ContainsRune(schema, 0) {
		return violation("schema contains a NUL byte")
	}
	if deniedSchemas[strings.ToLower(schema)] {
		return violation("schema %s is not allowed as the search_path: its functions are denied", schema)
	}
	return nil
}
