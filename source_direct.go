package main

import "context"

func init() { RegisterSource(&DirectSource{}) }

// DirectSource resolves credentials from an inline DSN string.
type DirectSource struct{}

func (DirectSource) Type() string { return "direct" }

func (DirectSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	v := newValidator(connName, "direct")
	v.require("dsn", conn.DSN)
	if err := v.err(); err != nil {
		return "", err
	}
	return conn.DSN, nil
}
