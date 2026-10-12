package guard

import "strings"

// CheckSearchPath returns a *Violation when schema, set as the search_path,
// would resolve unqualified names into a schema whose functions the guard
// denies whole, such as net: Check sees net.http_get, not where an
// unqualified http_get resolves.
func CheckSearchPath(schema string) error {
	// No function has an empty name, so only a rule on the schema can match.
	if isDenied([]string{strings.ToLower(schema), ""}) {
		return violation("schema %s is not allowed as the search path: the guard denies its functions", schema)
	}
	return nil
}
