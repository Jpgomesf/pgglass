package introspect_test

import (
	"context"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/introspect"
)

type renderer interface{ Render() string }

// The API other packages build against. Changing it must fail here first.
var (
	_ func(context.Context, engine.Querier) (introspect.SchemaList, error)                   = introspect.Schemas
	_ func(context.Context, engine.Querier, string) (introspect.RelationList, error)         = introspect.Tables
	_ func(context.Context, engine.Querier, string, string) (*introspect.TableDetail, error) = introspect.Table
	_ func(context.Context, engine.Querier, string, int) (introspect.MatchList, error)       = introspect.Search
	_ func(time.Duration) *introspect.Cache                                                  = introspect.NewCache
	_ func(*introspect.Cache, string, func() (string, error)) (string, error)                = (*introspect.Cache).Get

	_ renderer = introspect.SchemaList{{Name: "app", Relations: 7, Comment: "c"}}
	_ renderer = introspect.RelationList{{Schema: "app", Name: "orders", Kind: "table", Rows: 50, Comment: "c"}}
	_ renderer = introspect.MatchList{{Schema: "app", Table: "orders", Column: "total", Type: "numeric(12,2)"}}
	_ renderer = introspect.TableDetail{
		Relation:    introspect.Relation{Schema: "app", Name: "orders"},
		Columns:     []introspect.Column{{Name: "id", Type: "bigint", NotNull: true, Default: "", Comment: ""}},
		Indexes:     []introspect.Index{{Name: "orders_pkey", Definition: "CREATE UNIQUE INDEX ...", Primary: true, Unique: true}},
		Constraints: []introspect.Constraint{{Name: "orders_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"}},
		RLSEnabled:  true,
		Policies:    []introspect.Policy{{Name: "p", Command: "SELECT", Permissive: true, Roles: []string{"public"}, Using: "u", WithCheck: ""}},
		Definition:  "",
	}
)
