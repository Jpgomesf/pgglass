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
	// Exfiltration paths: dblink* opens sessions of its own, http* (pgsql-http)
	// and net.* (Supabase pg_net) send HTTP requests, and lo_export writes
	// server files, even in a read-only transaction.
	{SeverityCritical, []string{`%.dblink%`, `net.%`, `%.http%`, `%.lo\_export`}},
	// They read server files.
	{SeverityWarning, []string{`%.lo\_import`, `%.pg\_read\_file`}},
}

// Audit inspects, through q, the privileges of the role q connects as. A
// superuser gets a single finding, since it passes every other check. Audit
// returns an error, never a partial report, when a catalog query fails or
// comes back truncated.
func Audit(ctx context.Context, q engine.Querier) (Report, error) {
	attrs, err := queryAttributes(ctx, q)
	if err != nil {
		return Report{}, fmt.Errorf("preflight: role attributes: %w", err)
	}
	if attrs.superuser {
		return Report{Role: attrs.role, Findings: []Finding{
			{Severity: SeverityCritical, Check: "superuser", Detail: "bypasses every privilege check"},
		}}, nil
	}
	report := Report{Role: attrs.role, Findings: attrs.findings()}
	for _, check := range []struct {
		name string
		run  func(context.Context, engine.Querier) ([]Finding, error)
	}{
		{"role memberships", roleMemberships},
		{"table writes", tableWrites},
		{"function grants", functionGrants},
	} {
		found, err := check.run(ctx, q)
		if err != nil {
			return Report{}, fmt.Errorf("preflight: %s: %w", check.name, err)
		}
		report.Findings = append(report.Findings, found...)
	}
	// Critical findings first; each severity keeps the order of the checks.
	slices.SortStableFunc(report.Findings, func(a, b Finding) int {
		return severityRank(a.Severity) - severityRank(b.Severity)
	})
	return report, nil
}

func severityRank(severity string) int {
	if severity == SeverityCritical {
		return 0
	}
	return 1
}

const attributesSQL = `
SELECT current_user, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb
FROM pg_roles
WHERE rolname = current_user`

type attributes struct {
	role                                       string
	superuser, bypassRLS, createRole, createDB bool
}

func queryAttributes(ctx context.Context, q engine.Querier) (attributes, error) {
	row, err := queryRow(ctx, q, 5, attributesSQL)
	if err != nil {
		return attributes{}, err
	}
	a := attributes{role: text(row[0])}
	for i, flag := range []*bool{&a.superuser, &a.bypassRLS, &a.createRole, &a.createDB} {
		if *flag, err = strconv.ParseBool(text(row[i+1])); err != nil {
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

// membershipsSQL lists the roles in $1 the role is a member of. MEMBER, not
// USAGE: a member that does not inherit a role's privileges can still SET
// ROLE to it. Roles are matched by oid, so one missing on an older server is
// skipped rather than an error.
const membershipsSQL = `
SELECT string_agg(rolname, ' ' ORDER BY rolname)
FROM pg_roles
WHERE rolname = ANY ($1::text[]) AND pg_has_role(current_user, oid, 'MEMBER')`

func roleMemberships(ctx context.Context, q engine.Querier) ([]Finding, error) {
	row, err := queryRow(ctx, q, 1, membershipsSQL, slices.Sorted(maps.Keys(serverRoles)))
	if err != nil {
		return nil, err
	}
	var found []Finding
	for _, role := range strings.Fields(text(row[0])) {
		found = append(found, Finding{Severity: serverRoles[role], Check: "role_membership", Detail: "member of " + role})
	}
	return found, nil
}

// tableWritesSQL counts the tables, partitioned tables, views, materialized
// views and foreign tables the role can write, and lists the first $1.
// Column-level INSERT or UPDATE grants count. A relation in a schema the role
// cannot use is unreachable, so it does not count.
const tableWritesSQL = `
SELECT count(*), array_to_string((array_agg(privileges || ' on ' || name ORDER BY name))[1:$1::int], '; ')
FROM (
	SELECT format('%I.%I', n.nspname, c.relname) AS name,
	       concat_ws(', ',
	           CASE WHEN has_any_column_privilege(c.oid, 'INSERT') THEN 'INSERT' END,
	           CASE WHEN has_any_column_privilege(c.oid, 'UPDATE') THEN 'UPDATE' END,
	           CASE WHEN has_table_privilege(c.oid, 'DELETE') THEN 'DELETE' END,
	           CASE WHEN has_table_privilege(c.oid, 'TRUNCATE') THEN 'TRUNCATE' END) AS privileges
	FROM pg_class AS c
	JOIN pg_namespace AS n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND has_schema_privilege(n.oid, 'USAGE')
) AS relations
WHERE privileges <> ''`

func tableWrites(ctx context.Context, q engine.Querier) ([]Finding, error) {
	total, sample, err := countAndSample(ctx, q, tableWritesSQL, maxNames)
	if err != nil || total == 0 {
		return nil, err
	}
	return []Finding{{Severity: SeverityWarning, Check: "table_write", Detail: summarize(total, "relations", sample)}}, nil
}

// functionGrantsSQL counts the functions matching the patterns in $1 that the
// role can execute, overloads counted once, and lists the first $2. As with
// tables, a function in a schema the role cannot use does not count.
const functionGrantsSQL = `
SELECT count(*), array_to_string((array_agg(name ORDER BY name))[1:$2::int], ', ')
FROM (
	SELECT DISTINCT format('%I.%I', n.nspname, p.proname) AS name
	FROM pg_proc AS p
	JOIN pg_namespace AS n ON n.oid = p.pronamespace
	WHERE n.nspname || '.' || p.proname LIKE ANY ($1::text[])
	  AND has_function_privilege(p.oid, 'EXECUTE')
	  AND has_schema_privilege(n.oid, 'USAGE')
) AS functions`

func functionGrants(ctx context.Context, q engine.Querier) ([]Finding, error) {
	var found []Finding
	for _, group := range dangerousFunctions {
		total, sample, err := countAndSample(ctx, q, functionGrantsSQL, group.patterns, maxNames)
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
func queryRow(ctx context.Context, q engine.Querier, width int, sql string, params ...any) ([]*string, error) {
	res, err := q.Query(ctx, engine.Query{SQL: sql, Params: params})
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
