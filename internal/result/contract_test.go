package result_test

import "github.com/jpgomesf/mcp-postgres/internal/result"

// The API other packages build against. Changing it must fail here first.
var _ = result.Result{
	Columns:         []result.Column{{Name: "a", TypeOID: uint32(23), TypeName: "int4"}},
	Rows:            [][]*string{{nil}},
	Truncated:       true,
	TruncatedReason: "max_rows=1000",
}
