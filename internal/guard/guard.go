// Package guard rejects SQL that is not a single read-only statement, using
// the Postgres parser (libpg_query via github.com/wasilibs/go-pgquery, pure
// Go) rather than keyword matching. The engine's READ ONLY transaction stops
// writes; the guard also rejects what would act outside that transaction or
// survive its rollback, which READ ONLY cannot stop.
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

// Check returns nil if sql is exactly one statement that only reads, and a
// *Violation otherwise, including when sql does not parse.
//
// The statement must be a SELECT (VALUES, TABLE and set operations included),
// EXPLAIN [ANALYZE] of such a SELECT, or SHOW. Every node of the query is then
// checked: WITH queries must be SELECTs, SELECT INTO and FOR UPDATE/SHARE are
// rejected, and so are calls to the functions in deniedFunctions and
// deniedPrefixes, wherever they appear and including field-selection calls
// such as x.pg_read_file. SQL nested too deeply to parse safely is rejected
// before parsing. Check is safe for concurrent use.
func Check(sql string) error {
	stmt, err := parseSingle(sql)
	if err != nil {
		return err
	}
	return checkStatement(stmt)
}

// parseSingle parses sql and returns its only statement.
func parseSingle(sql string) (*pg_query.Node, error) {
	// The parser reads sql as a C string, so it would not see past a NUL.
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

// safely calls the parser's f, turning a panic of its WebAssembly runtime into
// an error. checkNesting keeps the known cause, a stack overflow, from
// happening.
func safely[T any](f func(string) (T, error), sql string) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser failed: %v", r)
		}
	}()
	return f(sql)
}

// parseErrorText is the parser's message, with the position when it has one.
func parseErrorText(err error) string {
	var perr *parser.Error
	if errors.As(err, &perr) && perr.Cursorpos > 0 {
		return fmt.Sprintf("%s at character %d", perr.Message, perr.Cursorpos)
	}
	return err.Error()
}

// checkStatement allows SELECT, EXPLAIN SELECT and SHOW, and checks every node
// of the SELECT.
func checkStatement(stmt *pg_query.Node) error {
	switch n := stmt.GetNode().(type) {
	case *pg_query.Node_SelectStmt:
		return walk(n.SelectStmt.ProtoReflect())
	case *pg_query.Node_ExplainStmt:
		// EXPLAIN options are constants in the grammar; only the query can hold
		// function calls. ANALYZE runs it, which is fine for a SELECT.
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

// nodeName names the type of node n wraps, e.g. "DeleteStmt".
func nodeName(n *pg_query.Node) string {
	name := "nothing"
	n.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		name = string(fd.Message().Name())
		return false
	})
	return name
}

// walk checks m and every message nested in it, through any field.
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

// walkField walks the messages that field fd holds in v.
func walkField(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch {
	case fd.Message() == nil:
		return nil
	case fd.IsMap():
		// pg_query's protobuf has no map fields; fail closed if one appears.
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

// checkNode checks one node of the tree.
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
	// Every statement node type is named *Stmt. Only SELECT may appear inside
	// a query; this rejects any other that the cases above did not.
	if name := string(m.ProtoReflect().Descriptor().Name()); strings.HasSuffix(name, "Stmt") {
		return violation("%s is not allowed inside a query", name)
	}
	return nil
}

// checkSelect rejects SELECT INTO, which creates a table, and FOR UPDATE or
// FOR SHARE, which lock rows.
func checkSelect(s *pg_query.SelectStmt) error {
	if s.GetIntoClause() != nil {
		return violation("SELECT INTO is not allowed: it creates a table")
	}
	if len(s.GetLockingClause()) > 0 {
		return violation("FOR UPDATE/FOR SHARE is not allowed: it locks rows")
	}
	return nil
}

// checkCTE rejects a WITH query that is not a SELECT, such as
// WITH gone AS (DELETE FROM t RETURNING *).
func checkCTE(c *pg_query.CommonTableExpr) error {
	if c.GetCtequery().GetSelectStmt() == nil {
		return violation("WITH query %q is a %s: only SELECT is allowed", c.GetCtename(), nodeName(c.GetCtequery()))
	}
	return nil
}

// checkRelation rejects a reference to a system view that parses and returns
// the content of a server configuration file. pg_read_file on those files is
// already denied; these views are the same read under another name, so they
// belong to the guard's "Server files" category. The match is on the
// relation's own name, lower-cased and whatever the schema, like
// checkFunction: a CTE or table that shadows one of these names is rejected
// too, since the parse tree cannot tell them apart.
func checkRelation(r *pg_query.RangeVar) error {
	if deniedRelations[strings.ToLower(r.GetRelname())] {
		return violation("relation %s is not allowed: it discloses a server configuration file", r.GetRelname())
	}
	return nil
}

// checkFunction rejects a call to a denied function. Names match
// case-insensitively, with or without a schema.
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

// Postgres also reads a field selection x.f as the call f(x) when x has no
// field f, so a denied function can be called without a FuncCall node:
// ('/etc/passwd'::text).pg_read_file, or t.pg_ls_dir FROM unnest(ARRAY['.'])
// AS t, whose t is a text. A qualified column that happens to share a denied
// name, such as t.lo_price, is rejected as well: the parse tree cannot tell.

// checkColumnRef checks rel.f: only the last name can be such a call.
func checkColumnRef(c *pg_query.ColumnRef) error {
	fields := c.GetFields()
	if len(fields) < 2 {
		return nil
	}
	return checkFieldCall(fields[len(fields)-1])
}

// checkIndirection checks (x).f.g: every field name can be such a call.
func checkIndirection(ind *pg_query.A_Indirection) error {
	for _, field := range ind.GetIndirection() {
		if err := checkFieldCall(field); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldCall rejects field selection .f when f is a denied function.
func checkFieldCall(field *pg_query.Node) error {
	name := strings.ToLower(field.GetString_().GetSval())
	if name != "" && isDenied([]string{name}) {
		return violation("function %s is not allowed: Postgres reads x.%s as %s(x)", name, name, name)
	}
	return nil
}

// isDenied reports whether the function named by parts, lower case and
// optionally schema-qualified, is denied.
func isDenied(parts []string) bool {
	if len(parts) == 0 {
		return true // the parser never produces this; fail closed
	}
	if len(parts) > 1 && parts[len(parts)-2] == "net" {
		return true // pg_net: HTTP requests from the server
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

// deniedFunctions reach outside the read-only transaction (server files,
// other sessions, settings, sequences, the network), leave state behind that
// its rollback does not undo, or run SQL passed as a string, which the guard
// cannot see.
var deniedFunctions = map[string]bool{
	// Server files.
	"pg_read_file":        true,
	"pg_read_binary_file": true,
	"pg_stat_file":        true,
	// Server configuration files, read through catalog functions. The matching
	// views (pg_hba_file_rules, pg_ident_file_mappings, pg_file_settings) are
	// denied as relations; see deniedRelations.
	"pg_show_all_file_settings": true, // postgresql.conf, behind the pg_file_settings view
	"pg_current_logfile":        true, // the active log file's path
	"pg_hba_file_rules":         true, // pg_hba.conf, where the view wraps a like-named function
	"pg_ident_file_mappings":    true, // pg_ident.conf, likewise
	// Other sessions and the server process.
	"pg_terminate_backend": true,
	"pg_cancel_backend":    true,
	"pg_reload_conf":       true,
	"pg_rotate_logfile":    true,
	"pg_promote":           true,
	// Settings and notifications.
	"set_config": true,
	"pg_notify":  true,
	// Sequences, which change outside the transaction.
	"setval":  true,
	"nextval": true,
	// Statistics resets, which are not transactional (pg_stat_statements).
	"pg_stat_statements_reset": true,
	// Backup control before PostgreSQL 15, and slot sync on a standby (17).
	"pg_start_backup":           true,
	"pg_stop_backup":            true,
	"pg_sync_replication_slots": true,
	// HTTP from the server (pgsql-http).
	"http":        true,
	"http_get":    true,
	"http_post":   true,
	"http_put":    true,
	"http_patch":  true,
	"http_delete": true,
	"http_head":   true,
	// SQL passed as a string.
	"query_to_xml":               true,
	"query_to_xml_and_xmlschema": true,
	"query_to_xmlschema":         true,
	"cursor_to_xml":              true,
	"cursor_to_xmlschema":        true,
	"ts_stat":                    true,
	"ts_rewrite":                 true, // ts_rewrite(query, select text) runs the select
	"connectby":                  true, // tablefunc: builds SQL from its text arguments
}

// deniedRelations are system views that parse and return the server's
// configuration files: the same disclosure as the denied pg_read_file, by
// another name. They are referenced as relations, not function calls, so the
// guard rejects them where it meets a RangeVar.
var deniedRelations = map[string]bool{
	"pg_hba_file_rules":      true, // pg_hba.conf, the authentication policy
	"pg_ident_file_mappings": true, // pg_ident.conf, the user name maps (PostgreSQL 15+)
	"pg_file_settings":       true, // postgresql.conf and its include files, with their paths
}

// deniedPrefixes deny whole function families by name prefix.
var deniedPrefixes = []string{
	"dblink",          // dblink, dblink_exec, ...: a second connection, outside the transaction
	"lo_",             // large objects; lo_import and lo_export reach server files
	"pg_logical_",     // logical decoding messages and slots
	"pg_log_",         // pg_log_standby_snapshot writes WAL; pg_log_backend_memory_contexts makes another backend log
	"pg_replication_", // replication origins and slots
	"pg_create_",      // replication slots and restore points
	"pg_drop_",        // replication slots
	"pg_backup_",      // backup control
	"pg_switch_",      // WAL segment switch
	"pg_copy_",        // copies of replication slots
	"pg_wal_replay_",  // pauses or resumes replay on a standby
	"pg_ls_",          // pg_ls_dir and the other server directory listings
	"pg_file_",        // adminpack (before PostgreSQL 17): writes, renames and removes server files
	"pg_stat_reset",   // statistics resets, which are not transactional
	// Session advisory locks: the rollback does not release them, so they stay
	// held on the pooled connection. Transaction-level ones (pg_advisory_xact_*)
	// are released with it.
	"pg_advisory_lock",
	"pg_try_advisory_lock",
	"database_to_xml", // database_to_xml*: runs a query per table of the database
	"schema_to_xml",   // schema_to_xml*: runs a query per table of the schema
	"crosstab",        // tablefunc: crosstab and its wrappers run SQL passed as a string
}
