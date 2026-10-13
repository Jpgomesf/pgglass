package testdb

// AppSQL returns the fixture SQL of DBApp, for tests that load a changed
// copy of it with CreateDatabase.
func AppSQL() string { return appSQL }
