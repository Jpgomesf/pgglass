package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

var appRelations = []resourceView{
	{Name: "audit_log", Description: "table"},
	{Name: "customer_order_totals", Description: "view"},
	{Name: "customers", Description: "table"},
	{Name: "daily_event_counts", Description: "materialized view"},
	{Name: "events", Description: "table"},
	{Name: "orders", Description: "table"},
	{Name: "tenant_notes", Description: "table"},
}

const superuserNotice = "role check: role postgres: 1 finding(s)\n  critical superuser: bypasses every privilege check\n\n"

type resourceView struct {
	URI, Name, Description, MIMEType string
}

func appResources(conn string) []resourceView {
	var views []resourceView
	for _, r := range appRelations {
		views = append(views, resourceView{
			URI:         "pgglass://" + conn + "/app/" + r.Name,
			Name:        conn + ": app." + r.Name,
			Description: r.Description,
			MIMEType:    "text/plain",
		})
	}
	return views
}

func (s *session) tableResources(t *testing.T) []resourceView {
	t.Helper()
	res, err := s.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if res.Resources == nil || res.NextCursor != "" {
		t.Errorf("resources/list = %+v, want one page with a list", res)
	}
	views := []resourceView{}
	for _, r := range res.Resources {
		views = append(views, resourceView{URI: r.URI, Name: r.Name, Description: r.Description, MIMEType: r.MIMEType})
	}
	return views
}

func resourceNames(views []resourceView) []string {
	names := []string{}
	for _, v := range views {
		names = append(names, v.Name)
	}
	return names
}

func (s *session) readTable(t *testing.T, uri string) string {
	t.Helper()
	res, err := s.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("resources/read %s: %v", uri, err)
	}
	if len(res.Contents) != 1 || res.Contents[0].URI != uri || res.Contents[0].MIMEType != "text/plain" {
		t.Fatalf("resources/read %s: contents %+v, want one text/plain item of that URI", uri, res.Contents)
	}
	return res.Contents[0].Text
}

func (s *session) readTableFails(t *testing.T, uri string) jsonrpc.Error {
	t.Helper()
	res, err := s.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("resources/read %s = %+v, %v; want a JSON-RPC error", uri, res, err)
	}
	return *wire
}

// resources/list opens no connection: it names the tables of the connections
// that tool calls and resource reads have used, by connection name.
func TestResourcesListTheTablesOfTheConnectionsInUseByName(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	if got := s.tableResources(t); len(got) != 0 || len(s.auditLines(t)) != 0 {
		t.Errorf("resources/list before any call = %v, audit log:\n%s\nwant no resources and no connection opened", got, s.audit)
	}
	s.ok(t, "pg_query", args("super", "SELECT 1 AS one"))
	if got, want := s.tableResources(t), appResources("super"); !reflect.DeepEqual(got, want) {
		t.Errorf("resources/list after a call on super:\n%v\nwant\n%v", got, want)
	}
	s.readTable(t, "pgglass://clean/app/orders")
	s.ok(t, "pg_schema", map[string]any{"connection": "leaky"})
	want := slices.Concat(appResources("clean"), appResources("leaky"), appResources("super"))
	if got := s.tableResources(t); !reflect.DeepEqual(got, want) {
		t.Errorf("resources/list after calls on every connection:\n%v\nwant\n%v", got, want)
	}
}

// When a call first uses a connection, the server tells the client that the
// resource list changed, on every protocol version, so that it lists the
// connection's tables.
func TestTheResourceListChangesWhenACallFirstUsesAConnection(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{"clean": appConns(t)["clean"]})
	for _, version := range []string{mcp.SupportedProtocolVersions()[0], "2025-11-25", "2024-11-05"} {
		changed := make(chan struct{}, 1)
		s := connectClient(t, cfg, t.TempDir(), version, &mcp.ClientOptions{
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				select {
				case changed <- struct{}{}:
				default:
				}
			},
		})
		if got := s.tableResources(t); len(got) != 0 {
			t.Errorf("%s: resources/list before any call = %v", version, got)
		}
		s.ok(t, "pg_query", args("clean", "SELECT 1 AS one"))
		select {
		case <-changed:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no notifications/resources/list_changed within 10s of the first call on clean", version)
		}
		if got, want := s.tableResources(t), appResources("clean"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: resources/list after the notification:\n%v\nwant\n%v", version, got, want)
		}
	}
}

// resources/list names the first relations of each connection in use, by
// schema and then name, as many as fit in an equal share of the inline limit;
// the template reads the others, and each audit line counts what it listed.
func TestResourceListingKeepsTheFirstRelationsOfEachConnectionThatFit(t *testing.T) {
	t.Parallel()
	const name = "tools_resources_cap"
	testdb.CreateDatabase(t, name, `CREATE SCHEMA a; CREATE SCHEMA b;
		DO $$ BEGIN FOR i IN 1..150 LOOP
			EXECUTE format('CREATE TABLE a.t%s (id int)', lpad(i::text, 3, '0'));
			EXECUTE format('CREATE TABLE b.t%s (id int)', lpad(i::text, 3, '0'));
		END LOOP; END $$;`)
	dsn := testdb.DSN(t, testdb.Superuser, name)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"one": direct(dsn), "two": direct(dsn)}))

	if got := sizes.ReplaceAllString(s.readTable(t, "pgglass://one/b/t150"), "SIZE"); got != "b.t150: table, SIZE\n  id integer" {
		t.Errorf("a relation past the listing, through the template:\n%s", got)
	}
	alone, size := s.listing(t)
	next, err := json.Marshal(&mcp.Resource{Description: "table", MIMEType: "text/plain", Name: "one: a.t999", URI: "pgglass://one/a/t999"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(alone, firstTables("one", len(alone))) || size > config.InlineMaxBytes || size+len(",")+len(next) < config.InlineMaxBytes {
		t.Errorf("resources/list of one connection, %d bytes of JSON: %d resources %v; want the first that fit in %d",
			size, len(alone), alone, config.InlineMaxBytes)
	}

	s.ok(t, "pg_schema", map[string]any{"connection": "two"})
	both, size := s.listing(t)
	half := len(both) / 2
	if !slices.Equal(both, slices.Concat(firstTables("one", half), firstTables("two", half))) || 2*half < len(alone)-1 || size > config.InlineMaxBytes {
		t.Errorf("resources/list of two connections, %d bytes of JSON: %v; want the first %d or so of each", size, both, len(alone)/2)
	}
	rows := map[string][]int{}
	for _, line := range s.auditLines(t) {
		if line.Tool == "resource_list" {
			rows[line.Connection] = append(rows[line.Connection], line.Rows)
		}
	}
	if want := map[string][]int{"one": {len(alone), half}, "two": {half}}; !reflect.DeepEqual(rows, want) {
		t.Errorf("resource_list audit rows = %v, want %v", rows, want)
	}
}

func (s *session) listing(t *testing.T) (names []string, jsonBytes int) {
	t.Helper()
	res, err := s.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Resources {
		names = append(names, r.Name)
	}
	return names, len(wire)
}

func firstTables(conn string, n int) []string {
	var names []string
	for i := range n {
		names = append(names, fmt.Sprintf("%s: a.t%03d", conn, i+1))
	}
	return names
}

func TestResourceTemplateNamesAnyTable(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckWarn, nil))
	if caps := s.InitializeResult().Capabilities; caps == nil || caps.Resources == nil {
		t.Errorf("capabilities = %+v, want resources", caps)
	}
	res, err := s.ListResourceTemplates(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ResourceTemplates) != 1 {
		t.Fatalf("%d resource templates, want 1", len(res.ResourceTemplates))
	}
	if tmpl := res.ResourceTemplates[0]; tmpl.URITemplate != "pgglass://{connection}/{schema}/{table}" || tmpl.MIMEType != "text/plain" ||
		tmpl.Name == "" || tmpl.Title == "" || tmpl.Description == "" {
		t.Errorf("resource template = %+v", tmpl)
	}
	if got := s.tableResources(t); len(got) != 0 {
		t.Errorf("resources without connections: %v", got)
	}
}

func TestReadingATableResourceReturnsWhatPgSchemaReturns(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, appConns(t))
	s, other := connect(t, cfg), connect(t, cfg)
	for _, r := range appRelations {
		want := other.ok(t, "pg_schema", map[string]any{"connection": "clean", "schema": "app", "table": r.Name})
		if got := s.readTable(t, "pgglass://clean/app/"+r.Name); got != want {
			t.Errorf("resource of app.%s:\n%s\nwant pg_schema's\n%s", r.Name, got, want)
		}
	}
}

func TestTableResourcesEscapeNamesThatNeedIt(t *testing.T) {
	t.Parallel()
	const name = "tools_resources_names"
	testdb.CreateDatabase(t, name, `CREATE SCHEMA "Odd Schema";
		CREATE TABLE "Odd Schema"."a/b c?#%" (id int);
		CREATE VIEW "Odd Schema"."café" AS SELECT 1 AS one;`)
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{"my db": direct(testdb.DSN(t, testdb.Superuser, name))})
	s, other := connect(t, cfg), connect(t, cfg)
	s.ok(t, "pg_schema", map[string]any{"connection": "my db"})

	want := []resourceView{
		{URI: "pgglass://my%20db/Odd%20Schema/a%2Fb%20c%3F%23%25", Name: "my db: Odd Schema.a/b c?#%", Description: "table", MIMEType: "text/plain"},
		{URI: "pgglass://my%20db/Odd%20Schema/caf%C3%A9", Name: "my db: Odd Schema.café", Description: "view", MIMEType: "text/plain"},
	}
	if got := s.tableResources(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("resources/list:\n%v\nwant\n%v", got, want)
	}
	for i, table := range []string{"a/b c?#%", "café"} {
		text := other.ok(t, "pg_schema", map[string]any{"connection": "my db", "schema": "Odd Schema", "table": table})
		if got := s.readTable(t, want[i].URI); got != text {
			t.Errorf("resource %s:\n%s\nwant pg_schema's\n%s", want[i].URI, got, text)
		}
	}
}

// Whether the server or the SDK finds that a URI names no table, the read is
// resource not found, and writes its audit line.
func TestReadingAMissingTableIsResourceNotFound(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, appConns(t)))
	uris := []string{
		"pgglass://nope/app/orders",
		"pgglass://clean/nope/orders",
		"pgglass://clean/app/nope",
		"pgglass://clean/app/" + strings.Repeat("t", 64),
		"pgglass://clean/app/%00",
		"pgglass://clean/app/%FF",
		"pgglass://clean/app",
		"pgglass://clean/app/orders/columns",
		"pgglass://clean/app/%zz",
		"postgres://clean/app/orders",
		"PGGLASS://clean/app/orders",
	}
	for _, uri := range uris {
		wire := s.readTableFails(t, uri)
		var data struct {
			URI string `json:"uri"`
		}
		if wire.Code != jsonrpc.CodeInvalidParams || wire.Message != "Resource not found" || json.Unmarshal(wire.Data, &data) != nil || data.URI != uri {
			t.Errorf("resources/read %s = %d %q %s, want resource not found", uri, wire.Code, wire.Message, wire.Data)
		}
	}
	lines := s.auditLines(t)
	for _, line := range lines {
		if line.Tool != "resource" || (line.Outcome != "error" && line.Outcome != "db_error") {
			t.Errorf("audit line %+v, want a failed resource read", line)
		}
	}
	if len(lines) != len(uris) {
		t.Errorf("%d audit lines for %d reads:\n%s", len(lines), len(uris), s.audit)
	}
}

func TestRoleCheckStrictKeepsRefusedConnectionsOutOfTableResources(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	for _, conn := range []string{"leaky", "super"} {
		wire := s.readTableFails(t, "pgglass://"+conn+"/app/orders")
		prefix := fmt.Sprintf("connection %q refused (role_check strict): its role can do more than read.\n", conn)
		if wire.Code != jsonrpc.CodeInternalError || !strings.HasPrefix(wire.Message, prefix) || !strings.Contains(wire.Message, " finding(s)\n  critical ") {
			t.Errorf("resources/read on %s = %d %q", conn, wire.Code, wire.Message)
		}
	}
	if got := s.readTable(t, "pgglass://clean/app/orders"); !strings.HasPrefix(got, "app.orders: table, ") {
		t.Errorf("resources/read on ro_clean:\n%s", got)
	}
	if got, want := s.tableResources(t), appResources("clean"); !reflect.DeepEqual(got, want) {
		t.Errorf("resources/list under role_check strict:\n%v\nwant\n%v", got, want)
	}
	outcomes := map[string]string{}
	for _, line := range s.auditLines(t) {
		outcomes[line.Tool+" "+line.Connection] = line.Outcome
	}
	if want := map[string]string{
		"resource_list clean": "ok", "resource_list leaky": "preflight_block", "resource_list super": "preflight_block",
		"resource leaky": "preflight_block", "resource super": "preflight_block", "resource clean": "ok",
	}; !reflect.DeepEqual(outcomes, want) {
		t.Errorf("audit outcomes = %v, want %v", outcomes, want)
	}
}

func TestTableResourceStaysWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const name = "tools_resources_wide"
	testdb.CreateDatabase(t, name, `DO $$ BEGIN
		EXECUTE 'CREATE TABLE wide (' || (SELECT string_agg(format('column_with_a_rather_long_name_%s text', lpad(i::text, 3, '0')), ', ')
			FROM generate_series(1, 300) i) || ')';
	END $$`)
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))}))
	for _, notice := range []string{superuserNotice, ""} {
		got := s.readTable(t, "pgglass://db/public/wide")
		note := fmt.Sprintf(" of 301 lines shown (output limit %d bytes): use pg_search to find names", config.InlineMaxBytes-len(notice))
		if len(got) > config.InlineMaxBytes || !strings.HasPrefix(got, notice+"public.wide: table, ") || !strings.HasSuffix(got, note) {
			t.Errorf("resource of a wide table, %d bytes:\n%.300s\n…\n%s", len(got), got, got[max(0, len(got)-200):])
		}
	}
}

func TestTableResourcesShowTheRoleCheckFindingsOnceUnderWarn(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))
	first := s.readTable(t, "pgglass://super/app/orders")
	again := s.readTable(t, "pgglass://super/app/orders")
	if first != superuserNotice+again || !strings.HasPrefix(again, "app.orders: table, ") {
		t.Errorf("first read:\n%s\nsecond read:\n%s\nwant the findings before the first only", first, again)
	}
	wire := s.readTableFails(t, "pgglass://leaky/app/nope")
	if !strings.HasPrefix(wire.Message, "role check: role ro_leaky: 3 finding(s)\n  critical function_execute: EXECUTE on ") ||
		!strings.HasSuffix(wire.Message, "\n\nResource not found") {
		t.Errorf("first read on ro_leaky, of a missing table = %q, want the findings first", wire.Message)
	}
	if got := s.ok(t, "pg_query", args("leaky", "SELECT 1 AS one")); got != "one\n1\n(1 row)" {
		t.Errorf("pg_query on ro_leaky repeats the findings:\n%s", got)
	}
}

func TestResourceListingSkipsAConnectionThatFails(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"clean": appConns(t)["clean"],
		"down":  direct(unusedDSN),
	}))
	s.ok(t, "pg_query", args("clean", "SELECT 1 AS one"))
	s.fails(t, "pg_query", args("down", "SELECT 1 AS one"))
	if got, want := s.tableResources(t), appResources("clean"); !reflect.DeepEqual(got, want) {
		t.Errorf("resources/list with a connection down:\n%v\nwant\n%v", got, want)
	}
	outcomes := map[string]string{}
	for _, line := range s.auditLines(t) {
		outcomes[line.Tool+" "+line.Connection] = line.Outcome
	}
	if want := map[string]string{
		"pg_query clean": "ok", "pg_query down": "db_error", "resource_list clean": "ok", "resource_list down": "db_error",
	}; !reflect.DeepEqual(outcomes, want) {
		t.Errorf("audit outcomes = %v, want %v", outcomes, want)
	}
}

func TestResourceListIsCached(t *testing.T) {
	t.Parallel()
	const name = "tools_resources_cache"
	testdb.CreateDatabase(t, name, "CREATE TABLE before_listing (id int)")
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"db": direct(testdb.DSN(t, testdb.Superuser, name))}))
	s.ok(t, "pg_schema", map[string]any{"connection": "db"})
	want := []string{"db: public.before_listing"}
	if got := resourceNames(s.tableResources(t)); !slices.Equal(got, want) {
		t.Fatalf("first listing: %v", got)
	}
	if _, err := testdb.Conn(t, testdb.Superuser, name).Exec(t.Context(), "CREATE TABLE after_listing (id int)"); err != nil {
		t.Fatal(err)
	}
	if got := resourceNames(s.tableResources(t)); !slices.Equal(got, want) {
		t.Errorf("second listing changed within the cache TTL: %v", got)
	}
	if got := s.readTable(t, "pgglass://db/public/after_listing"); !strings.HasPrefix(got, "public.after_listing: table") {
		t.Errorf("the new table, through the template:\n%s", got)
	}
}

func TestTableResourcesWorkOnEveryProtocolVersion(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{"clean": appConns(t)["clean"]})
	for _, version := range mcp.SupportedProtocolVersions() {
		s := connectAt(t, cfg, t.TempDir(), version)
		if res, err := s.ListResourceTemplates(t.Context(), nil); err != nil || len(res.ResourceTemplates) != 1 {
			t.Errorf("%s: resources/templates/list = %+v, %v", version, res, err)
		}
		if got := s.readTable(t, "pgglass://clean/app/orders"); !strings.HasPrefix(got, "app.orders: table, ") {
			t.Errorf("%s: resources/read:\n%s", version, got)
		}
		if got, want := s.tableResources(t), appResources("clean"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: resources/list:\n%v", version, got)
		}
	}
}

func TestTableResourcesWriteAuditLinesWithoutSQL(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"clean": appConns(t)["clean"]}))
	s.readTable(t, "pgglass://clean/app/orders")
	s.tableResources(t)
	s.readTableFails(t, "pgglass://clean/app/nope")
	s.readTableFails(t, "pgglass://nope/app/orders")
	s.readTableFails(t, "pgglass://clean/app/orders/columns")

	want := []auditLine{
		{Msg: "tool call", Tool: "resource", Connection: "clean", Outcome: "ok"},
		{Msg: "tool call", Tool: "resource_list", Connection: "clean", Rows: 7, Outcome: "ok"},
		{Msg: "tool call", Tool: "resource", Connection: "clean", Outcome: "db_error"},
		{Msg: "tool call", Tool: "resource", Outcome: "error"},
		{Msg: "tool call", Tool: "resource", Outcome: "error"},
	}
	lines := s.auditLines(t)
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(want), s.audit)
	}
	for i, line := range lines {
		if line.DurationMS == nil || *line.DurationMS < 0 {
			t.Errorf("line %d: duration_ms = %v", i, line.DurationMS)
		}
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("line %d = %+v\nwant     %+v", i, line, want[i])
		}
	}
}
