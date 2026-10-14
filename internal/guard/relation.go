package guard

import "strings"

// CheckRelation returns a *Violation if Check rejects references to the
// relation name. Tools that read a relation named outside SQL must check it.
func CheckRelation(name string) error {
	if strings.ContainsRune(name, 0) {
		return violation("relation name contains a NUL byte")
	}
	if deniedRelations[strings.ToLower(name)] {
		return violation("relation %s is not allowed: it discloses server configuration or credentials", name)
	}
	return nil
}
