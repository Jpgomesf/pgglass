package main

import (
	"fmt"
	"strings"
)

// fieldValidator collects missing or empty fields and reports them all at once.
type fieldValidator struct {
	connName   string
	sourceType string
	missing    []string
}

func newValidator(connName, sourceType string) *fieldValidator {
	return &fieldValidator{connName: connName, sourceType: sourceType}
}

// require checks that value is non-empty. Returns the value for convenient chaining.
func (v *fieldValidator) require(field, value string) string {
	if value == "" {
		v.missing = append(v.missing, field)
	}
	return value
}

// requireFromMap checks that a key exists and is non-empty in the map.
func (v *fieldValidator) requireFromMap(m map[string]string, key string) string {
	val := m[key]
	if val == "" {
		v.missing = append(v.missing, key)
	}
	return val
}

// err returns a combined error if any fields are missing, nil otherwise.
func (v *fieldValidator) err() error {
	if len(v.missing) == 0 {
		return nil
	}
	return fmt.Errorf("connection %q (%s): missing required fields: %s",
		v.connName, v.sourceType, strings.Join(v.missing, ", "))
}
