// Package preflight audits what the role behind a connection can do beyond
// reading, so a connection that is not truly read-only is refused or flagged
// (config role_check: strict, warn or off).
package preflight

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

// Finding severities.
const (
	SeverityCritical = "critical" // the role can change data or escape the read-only transaction
	SeverityWarning  = "warning"  // risky, but not a write path on its own
)

// Finding is one capability the role should not have.
type Finding struct {
	Severity string // SeverityCritical or SeverityWarning
	Check    string // short identifier of the check, e.g. "table_write"
	Detail   string // what was found, e.g. "INSERT on app.audit_log"
}

// Report is the result of Audit.
type Report struct {
	Role     string // current_user of the audited connection
	Findings []Finding
}

// Blocking reports whether any finding is critical.
func (r Report) Blocking() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// String renders the report for tool output: one line naming the role,
// then one line per finding.
func (r Report) String() string {
	if len(r.Findings) == 0 {
		return fmt.Sprintf("role %s: no findings", r.Role)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "role %s: %d finding(s)", r.Role, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "\n  %s %s: %s", f.Severity, f.Check, f.Detail)
	}
	return b.String()
}

// maxNames bounds how many relations or functions one finding names.
const maxNames = 10

// serverRoles are the predefined roles whose membership is a finding.
var serverRoles = map[string]string{
	"pg_execute_server_program": SeverityCritical, // COPY ... PROGRAM runs shell commands
	"pg_write_server_files":     SeverityCritical, // COPY ... TO writes server files
	"pg_read_server_files":      SeverityWarning,  // COPY ... FROM reads server files
	"pg_signal_backend":         SeverityWarning,  // cancels or terminates other sessions
}

// dangerousFunctions are the functions whose EXECUTE privilege is a finding,
// as LIKE patterns on "schema.function", by severity.
var dangerousFunctions = []struct {
	severity string
	patterns []string
}{
	// Ways out of the read-only transaction: dblink* opens sessions of its
	// own, pg_background* runs SQL in a worker's own transaction, http*
	// (pgsql-http), net.* (Supabase pg_net), aws_s3.* and aws_lambda.* (RDS)
	// send requests or rows out, and lo_export writes server files.
	{SeverityCritical, []string{`%.dblink%`, `%.pg\_background%`, `%.http%`, `net.%`, `aws\_s3.%`, `aws\_lambda.%`, `%.lo\_export`}},
	// They read server files, or make another backend dump its memory
	// contexts to the server log.
	{SeverityWarning, []string{`%.lo\_import`, `%.pg\_read\_file`, `%.pg\_read\_binary\_file`, `%.pg\_log\_backend\_memory\_contexts`}},
}

// Audit inspects, through q, the privileges of the role q connects as: the
// current user and, when the connection set another role, the session user
// too, which RESET ROLE returns to. Each superuser gets a single finding,
// since it passes every other check. Audit returns an error, never a partial
// report, when a catalog query fails or comes back truncated.
func Audit(ctx context.Context, q engine.Querier) (Report, error) {
	users, err := queryRow(ctx, q, 2, usersSQL)
	if err != nil {
		return Report{}, fmt.Errorf("preflight: current and session user: %w", err)
	}
	report := Report{Role: text(users[0])}
	if report.Findings, err = auditRole(ctx, q, report.Role); err != nil {
		return Report{}, err
	}
	if session := text(users[1]); session != report.Role {
		found, err := auditRole(ctx, q, session)
		if err != nil {
			return Report{}, err
		}
		for _, f := range found {
			f.Detail = "session user " + session + ": " + f.Detail
			report.Findings = append(report.Findings, f)
		}
	}
	// Critical findings first; each severity keeps the order of the checks.
	slices.SortStableFunc(report.Findings, func(a, b Finding) int {
		return severityRank(a.Severity) - severityRank(b.Severity)
	})
	return report, nil
}

const usersSQL = `SELECT current_user, session_user`

// auditRole returns the findings for role.
func auditRole(ctx context.Context, q engine.Querier, role string) ([]Finding, error) {
	attrs, err := queryAttributes(ctx, q, role)
	if err != nil {
		return nil, fmt.Errorf("preflight: role attributes: %w", err)
	}
	if attrs.superuser {
		return []Finding{{Severity: SeverityCritical, Check: "superuser", Detail: "bypasses every privilege check"}}, nil
	}
	found := attrs.findings()
	for _, check := range []struct {
		name string
		run  func(context.Context, engine.Querier, string) ([]Finding, error)
	}{
		{"role memberships", roleMemberships},
		{"superuser memberships", superuserMemberships},
		{"table writes", tableWrites},
		{"function grants", functionGrants},
	} {
		more, err := check.run(ctx, q, role)
		if err != nil {
			return nil, fmt.Errorf("preflight: %s: %w", check.name, err)
		}
		found = append(found, more...)
	}
	return found, nil
}

func severityRank(severity string) int {
	if severity == SeverityCritical {
		return 0
	}
	return 1
}

const attributesSQL = `
SELECT rolsuper, rolbypassrls, rolcreaterole, rolcreatedb
FROM pg_roles
WHERE rolname = $1`

type attributes struct {
	superuser, bypassRLS, createRole, createDB bool
}

func queryAttributes(ctx context.Context, q engine.Querier, role string) (attributes, error) {
	row, err := queryRow(ctx, q, 4, attributesSQL, role)
	if err != nil {
		return attributes{}, err
	}
	var a attributes
	for i, flag := range []*bool{&a.superuser, &a.bypassRLS, &a.createRole, &a.createDB} {
		if *flag, err = strconv.ParseBool(text(row[i])); err != nil {
			return attributes{}, err
		}
	}
	return a, nil
}

// findings returns the warnings for the role attributes other than superuser,
// which Audit handles.
func (a attributes) findings() []Finding {
	var found []Finding
	for _, attr := range []struct {
		held          bool
		check, detail string
	}{
		{a.bypassRLS, "bypassrls", "ignores row-level security"},
		{a.createRole, "createrole", "can create roles"},
		{a.createDB, "createdb", "can create databases"},
	} {
		if attr.held {
			found = append(found, Finding{Severity: SeverityWarning, Check: attr.check, Detail: attr.detail})
		}
	}
	return found
}

// membershipsSQL lists the roles in $2 that role $1 is a member of. MEMBER,
// not USAGE: a member that does not inherit a role's privileges can still SET
// ROLE to it. Roles are matched by oid, so one missing on an older server is
// skipped rather than an error.
const membershipsSQL = `
SELECT string_agg(rolname, ' ' ORDER BY rolname)
FROM pg_roles
WHERE rolname = ANY ($2::text[]) AND pg_has_role($1::name, oid, 'MEMBER')`

func roleMemberships(ctx context.Context, q engine.Querier, role string) ([]Finding, error) {
	row, err := queryRow(ctx, q, 1, membershipsSQL, role, slices.Sorted(maps.Keys(serverRoles)))
	if err != nil {
		return nil, err
	}
	var found []Finding
	for _, granted := range strings.Fields(text(row[0])) {
		found = append(found, Finding{Severity: serverRoles[granted], Check: "role_membership", Detail: "member of " + granted})
	}
	return found, nil
}

// superusersSQL counts the superusers that role $1 is a member of, and lists
// the first $2. Superuser is never inherited, but SET ROLE reaches it, as for
// the predefined roles.
const superusersSQL = `
SELECT count(*), array_to_string((array_agg(format('%I', rolname) ORDER BY rolname))[1:$2::int], ', ')
FROM pg_roles
WHERE rolsuper AND pg_has_role($1::name, oid, 'MEMBER')`

func superuserMemberships(ctx context.Context, q engine.Querier, role string) ([]Finding, error) {
	total, sample, err := countAndSample(ctx, q, superusersSQL, role, maxNames)
	if err != nil || total == 0 {
		return nil, err
	}
	return []Finding{{Severity: SeverityCritical, Check: "superuser_membership", Detail: "member of " + summarize(total, "superusers", sample)}}, nil
}

// tableWritesSQL counts the tables, partitioned tables, views, materialized
// views and foreign tables that role $1 can write, and lists the first $2.
// Column-level INSERT or UPDATE grants count. A relation in a schema the role
// cannot use is unreachable, so it does not count.
const tableWritesSQL = `
SELECT count(*), array_to_string((array_agg(privileges || ' on ' || name ORDER BY name))[1:$2::int], '; ')
FROM (
	SELECT format('%I.%I', n.nspname, c.relname) AS name,
	       concat_ws(', ',
	           CASE WHEN has_any_column_privilege($1::name, c.oid, 'INSERT') THEN 'INSERT' END,
	           CASE WHEN has_any_column_privilege($1::name, c.oid, 'UPDATE') THEN 'UPDATE' END,
	           CASE WHEN has_table_privilege($1::name, c.oid, 'DELETE') THEN 'DELETE' END,
	           CASE WHEN has_table_privilege($1::name, c.oid, 'TRUNCATE') THEN 'TRUNCATE' END) AS privileges
	FROM pg_class AS c
	JOIN pg_namespace AS n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND has_schema_privilege($1::name, n.oid, 'USAGE')
) AS relations
WHERE privileges <> ''`

func tableWrites(ctx context.Context, q engine.Querier, role string) ([]Finding, error) {
	total, sample, err := countAndSample(ctx, q, tableWritesSQL, role, maxNames)
	if err != nil || total == 0 {
		return nil, err
	}
	return []Finding{{Severity: SeverityWarning, Check: "table_write", Detail: summarize(total, "relations", sample)}}, nil
}

// functionGrantsSQL counts the functions matching the patterns in $2 that
// role $1 can execute, overloads counted once, and lists the first $3. As
// with tables, a function in a schema the role cannot use does not count.
const functionGrantsSQL = `
SELECT count(*), array_to_string((array_agg(name ORDER BY name))[1:$3::int], ', ')
FROM (
	SELECT DISTINCT format('%I.%I', n.nspname, p.proname) AS name
	FROM pg_proc AS p
	JOIN pg_namespace AS n ON n.oid = p.pronamespace
	WHERE n.nspname || '.' || p.proname LIKE ANY ($2::text[])
	  AND has_function_privilege($1::name, p.oid, 'EXECUTE')
	  AND has_schema_privilege($1::name, n.oid, 'USAGE')
) AS functions`

func functionGrants(ctx context.Context, q engine.Querier, role string) ([]Finding, error) {
	var found []Finding
	for _, group := range dangerousFunctions {
		total, sample, err := countAndSample(ctx, q, functionGrantsSQL, role, group.patterns, maxNames)
		if err != nil {
			return nil, err
		}
		if total > 0 {
			detail := "EXECUTE on " + summarize(total, "functions", sample)
			found = append(found, Finding{Severity: group.severity, Check: "function_execute", Detail: detail})
		}
	}
	return found, nil
}

// summarize describes total items from sample, which lists the first maxNames.
func summarize(total int, noun, sample string) string {
	switch {
	case total == 1:
		return sample
	case total <= maxNames:
		return fmt.Sprintf("%d %s: %s", total, noun, sample)
	default:
		return fmt.Sprintf("%d %s, first %d: %s", total, noun, maxNames, sample)
	}
}

// countAndSample runs sql, whose one row holds a count and a sample of names.
func countAndSample(ctx context.Context, q engine.Querier, sql string, params ...any) (int, string, error) {
	row, err := queryRow(ctx, q, 2, sql, params...)
	if err != nil {
		return 0, "", err
	}
	total, err := strconv.Atoi(text(row[0]))
	return total, text(row[1]), err
}

// queryRow runs sql and returns its only row, which must hold width values.
// Every query of the audit aggregates into one row, so max_rows cannot cut it
// short, and any other result is an error rather than a silently clean audit.
// The search_path is pg_catalog alone: a schema the role or database puts
// before it could otherwise shadow the catalogs and functions the audit reads
// and fake a clean report.
func queryRow(ctx context.Context, q engine.Querier, width int, sql string, params ...any) ([]*string, error) {
	res, err := q.Query(ctx, engine.Query{SQL: sql, Params: params, SearchPath: "pg_catalog"})
	if err != nil {
		return nil, err
	}
	switch {
	case res == nil:
		return nil, errors.New("no result")
	case res.Truncated:
		return nil, fmt.Errorf("result truncated (%s)", res.TruncatedReason)
	case len(res.Rows) != 1 || len(res.Rows[0]) != width:
		return nil, fmt.Errorf("want 1 row of %d values, got %d rows", width, len(res.Rows))
	}
	return res.Rows[0], nil
}

// text returns a value, with NULL as "".
func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
