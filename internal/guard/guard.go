// Package guard rejects SQL that is not a single read-only statement, using
// the Postgres parser (libpg_query via github.com/wasilibs/go-pgquery, pure
// Go) rather than keyword matching. The engine's READ ONLY transaction stops
// writes; the guard also rejects what would act outside that transaction or
// survive its rollback, which READ ONLY cannot stop, and reads of the
// catalogs that hold server configuration files or credentials.
//
// The parser lexes SQL like a server with standard_conforming_strings on, the
// default since PostgreSQL 9.1. With it off, a backslash in a plain '...'
// literal can move where the literal ends, so the engine must not run checked
// SQL in such a session.
package guard

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"github.com/wasilibs/go-pgquery/parser"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Violation explains why SQL was rejected.
type Violation struct {
	Reason string
}

// Error returns the reason.
func (v *Violation) Error() string {
	return v.Reason
}

func violation(format string, args ...any) error {
	return &Violation{Reason: fmt.Sprintf(format, args...)}
}

// Check returns nil if sql is one read-only SELECT, EXPLAIN of one, or SHOW
// that touches no denied relation or function, and a *Violation otherwise.
func Check(sql string) error {
	stmt, err := parseSingle(sql)
	if err != nil {
		return err
	}
	return checkStatement(stmt)
}

func parseSingle(sql string) (*pg_query.Node, error) {
	if strings.ContainsRune(sql, 0) {
		return nil, violation("SQL contains a NUL byte")
	}
	if !utf8.ValidString(sql) {
		return nil, violation("SQL is not valid UTF-8")
	}
	if err := checkNesting(sql); err != nil {
		return nil, err
	}
	tree, err := safely(pgquery.Parse, sql)
	if err != nil {
		return nil, violation("SQL does not parse: %s", parseErrorText(err))
	}
	stmts := tree.GetStmts()
	if len(stmts) != 1 {
		return nil, violation("SQL must be exactly one statement, got %d", len(stmts))
	}
	return stmts[0].GetStmt(), nil
}

func safely[T any](f func(string) (T, error), sql string) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser failed: %v", r)
		}
	}()
	return f(sql)
}

func parseErrorText(err error) string {
	var perr *parser.Error
	if errors.As(err, &perr) && perr.Cursorpos > 0 {
		return fmt.Sprintf("%s at character %d", perr.Message, perr.Cursorpos)
	}
	return err.Error()
}

func checkStatement(stmt *pg_query.Node) error {
	switch n := stmt.GetNode().(type) {
	case *pg_query.Node_SelectStmt:
		return walk(n.SelectStmt.ProtoReflect())
	case *pg_query.Node_ExplainStmt:
		query := n.ExplainStmt.GetQuery()
		if query.GetSelectStmt() == nil {
			return violation("EXPLAIN of %s is not allowed: only a SELECT can be explained", nodeName(query))
		}
		return walk(query.GetSelectStmt().ProtoReflect())
	case *pg_query.Node_VariableShowStmt:
		return nil
	default:
		return violation("%s is not allowed: only SELECT, EXPLAIN SELECT and SHOW are", nodeName(stmt))
	}
}

func nodeName(n *pg_query.Node) string {
	name := "nothing"
	n.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		name = string(fd.Message().Name())
		return false
	})
	return name
}

func walk(m protoreflect.Message) error {
	if err := checkNode(m.Interface()); err != nil {
		return err
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		err = walkField(fd, v)
		return err == nil
	})
	return err
}

func walkField(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch {
	case fd.Message() == nil:
		return nil
	case fd.IsMap():
		return violation("cannot check %s", fd.FullName())
	case fd.IsList():
		list := v.List()
		for i := range list.Len() {
			if err := walk(list.Get(i).Message()); err != nil {
				return err
			}
		}
		return nil
	default:
		return walk(v.Message())
	}
}

func checkNode(m protoreflect.ProtoMessage) error {
	switch n := m.(type) {
	case *pg_query.FuncCall:
		return checkFunction(n.GetFuncname())
	case *pg_query.RangeVar:
		return checkRelation(n)
	case *pg_query.ColumnRef:
		return checkColumnRef(n)
	case *pg_query.A_Indirection:
		return checkIndirection(n)
	case *pg_query.SelectStmt:
		return checkSelect(n)
	case *pg_query.CommonTableExpr:
		return checkCTE(n)
	}
	if name := string(m.ProtoReflect().Descriptor().Name()); strings.HasSuffix(name, "Stmt") {
		return violation("%s is not allowed inside a query", name)
	}
	return nil
}

func checkSelect(s *pg_query.SelectStmt) error {
	if s.GetIntoClause() != nil {
		return violation("SELECT INTO is not allowed: it creates a table")
	}
	if len(s.GetLockingClause()) > 0 {
		return violation("FOR UPDATE/FOR SHARE is not allowed: it locks rows")
	}
	return nil
}

func checkCTE(c *pg_query.CommonTableExpr) error {
	if c.GetCtequery().GetSelectStmt() == nil {
		return violation("WITH query %q is a %s: only SELECT is allowed", c.GetCtename(), nodeName(c.GetCtequery()))
	}
	return nil
}

func checkRelation(r *pg_query.RangeVar) error {
	return CheckRelation(r.GetRelname())
}

func checkFunction(name []*pg_query.Node) error {
	parts := make([]string, len(name))
	for i, part := range name {
		parts[i] = strings.ToLower(part.GetString_().GetSval())
	}
	if isDenied(parts) {
		return violation("function %s is not allowed", strings.Join(parts, "."))
	}
	return nil
}

func checkColumnRef(c *pg_query.ColumnRef) error {
	fields := c.GetFields()
	if len(fields) < 2 {
		return nil
	}
	return checkFieldCall(fields[len(fields)-1])
}

func checkIndirection(ind *pg_query.A_Indirection) error {
	for _, field := range ind.GetIndirection() {
		if err := checkFieldCall(field); err != nil {
			return err
		}
	}
	return nil
}

func checkFieldCall(field *pg_query.Node) error {
	name := strings.ToLower(field.GetString_().GetSval())
	if name != "" && isDenied([]string{name}) {
		return violation("function %s is not allowed: Postgres reads x.%s as %s(x)", name, name, name)
	}
	return nil
}

func isDenied(parts []string) bool {
	if len(parts) == 0 {
		return true
	}
	if len(parts) > 1 && deniedSchemas[parts[len(parts)-2]] {
		return true
	}
	name := parts[len(parts)-1]
	if deniedFunctions[name] {
		return true
	}
	for _, prefix := range deniedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var deniedSchemas = map[string]bool{
	"net":        true,
	"aws_s3":     true,
	"aws_lambda": true,
}

var deniedFunctions = map[string]bool{
	"pg_read_file":                true,
	"pg_read_binary_file":         true,
	"pg_stat_file":                true,
	"pg_show_all_file_settings":   true,
	"pg_current_logfile":          true,
	"pg_hba_file_rules":           true,
	"pg_ident_file_mappings":      true,
	"pg_terminate_backend":        true,
	"pg_cancel_backend":           true,
	"pg_reload_conf":              true,
	"pg_rotate_logfile":           true,
	"pg_promote":                  true,
	"set_config":                  true,
	"pg_notify":                   true,
	"setseed":                     true,
	"pg_prewarm":                  true,
	"setval":                      true,
	"nextval":                     true,
	"pg_stat_statements_reset":    true,
	"pg_start_backup":             true,
	"pg_stop_backup":              true,
	"pg_sync_replication_slots":   true,
	"http":                        true,
	"http_get":                    true,
	"http_post":                   true,
	"http_put":                    true,
	"http_patch":                  true,
	"http_delete":                 true,
	"http_head":                   true,
	"query_to_xml":                true,
	"query_to_xml_and_xmlschema":  true,
	"query_to_xmlschema":          true,
	"cursor_to_xml":               true,
	"cursor_to_xmlschema":         true,
	"ts_stat":                     true,
	"ts_rewrite":                  true,
	"connectby":                   true,
	"xpath_table":                 true,
	"get_raw_page":                true,
	"pg_truncate_visibility_map":  true,
	"brin_summarize_new_values":   true,
	"brin_summarize_range":        true,
	"brin_desummarize_range":      true,
	"gin_clean_pending_list":      true,
	"pg_nextoid":                  true,
	"txid_current":                true,
	"pg_current_xact_id":          true,
	"pg_import_system_collations": true,
}

var deniedRelations = map[string]bool{
	"pg_hba_file_rules":      true,
	"pg_ident_file_mappings": true,
	"pg_file_settings":       true,
	"pg_authid":              true,
	"pg_shadow":              true,
	"pg_user_mapping":        true,
	"pg_user_mappings":       true,
	"user_mapping_options":   true,
	"_pg_user_mappings":      true,
	"pg_subscription":        true,
}

var deniedPrefixes = []string{
	"dblink",
	"pg_background",
	"lo_",
	"pg_logical_",
	"pg_log_",
	"pg_replication_",
	"pg_create_",
	"pg_drop_",
	"pg_backup_",
	"pg_switch_",
	"pg_copy_",
	"pg_wal_replay_",
	"pg_ls_",
	"pg_file_",
	"pg_stat_reset",
	"pg_advisory_lock",
	"pg_advisory_xact_lock",
	"pg_try_advisory_lock",
	"pg_try_advisory_xact_lock",
	"database_to_xml",
	"schema_to_xml",
	"table_to_xml",
	"crosstab",
	"heap_force_",
	"autoprewarm_",
	"pg_restore_",
	"pg_clear_",
	"pg_buffercache_evict",
	"pg_get_logical_snapshot_",
}
