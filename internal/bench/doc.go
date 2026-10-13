// Package bench measures what pgglass puts in an agent's context: the tool
// definitions of tools/list, the instructions of initialize, and the
// response to each of ten agent questions asked of the testdb fixture
// through the MCP in-memory transport. Sizes are in bytes, with tokens
// estimated as bytes/4.
//
// TestTokenBudget fails when a size grows more than 10% past
// testdata/bench-baseline.json or tools/list passes its cap. With -update,
// it first rewrites that file and the table in docs/benchmark.md.
package bench
