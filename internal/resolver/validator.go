package resolver

import (
	"fmt"
	"strings"
)

// FieldValidator collects missing or empty fields and reports them all at once.
type FieldValidator struct {
	connName   string
	sourceType string
	missing    []string
}

// NewValidator creates a validator scoped to a connection and source type.
func NewValidator(connName, sourceType string) *FieldValidator {
	return &FieldValidator{connName: connName, sourceType: sourceType}
}

// Require checks that value is non-empty. Returns the value for convenient chaining.
func (v *FieldValidator) Require(field, value string) string {
	if value == "" {
		v.missing = append(v.missing, field)
	}
	return value
}

// RequireFromMap checks that a key exists and is non-empty in the map.
func (v *FieldValidator) RequireFromMap(m map[string]string, key string) string {
	val := m[key]
	if val == "" {
		v.missing = append(v.missing, key)
	}
	return val
}

// Err returns a combined error if any fields are missing, nil otherwise.
func (v *FieldValidator) Err() error {
	if len(v.missing) == 0 {
		return nil
	}
	return fmt.Errorf("connection %q (%s): missing required fields: %s",
		v.connName, v.sourceType, strings.Join(v.missing, ", "))
}
