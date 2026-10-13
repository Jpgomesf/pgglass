package guard

import "strings"

// CheckRelation returns a *Violation if name, a relation's own name, is one
// whose references Check rejects: a catalog or view that discloses server
// configuration files or credentials. Tools that read a relation named
// outside SQL, such as through its statistics, must check the name with it.
func CheckRelation(name string) error {
	// pgx drops NUL bytes when it quotes a name: "pg_au\x00thid" would name
	// pg_authid.
	if strings.ContainsRune(name, 0) {
		return violation("relation name contains a NUL byte")
	}
	if deniedRelations[strings.ToLower(name)] {
		return violation("relation %s is not allowed: it discloses server configuration or credentials", name)
	}
	return nil
}
