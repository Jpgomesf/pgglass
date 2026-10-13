package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

type user struct {
	action string

	mu    sync.Mutex
	asked []*mcp.ElicitParams
}

func (u *user) answer(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asked = append(u.asked, req.Params)
	return &mcp.ElicitResult{Action: u.action}, nil
}

func (u *user) forms() []*mcp.ElicitParams {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.asked)
}

func (u *user) questions() []string {
	var messages []string
	for _, p := range u.forms() {
		messages = append(messages, p.Message)
	}
	return messages
}

func connectUser(t *testing.T, cfg config.Config, protocolVersion, action string) (*session, *user) {
	t.Helper()
	u := &user{action: action}
	opts := &mcp.ClientOptions{}
	if action != "" {
		opts.ElicitationHandler = u.answer
	}
	return connectClient(t, cfg, t.TempDir(), protocolVersion, opts), u
}

func confirmed(dsn string) config.Connection {
	return config.Connection{Source: "direct", DSN: dsn, Confirm: true}
}

func (s *session) auditedCall(t *testing.T, tool string, a map[string]any) (string, bool, []auditLine) {
	t.Helper()
	before := len(s.auditLines(t))
	text, isErr := s.call(t, tool, a)
	lines := s.auditLines(t)[before:]
	for i := range lines {
		if lines[i].DurationMS == nil || *lines[i].DurationMS < 0 {
			t.Errorf("audit line %+v: duration_ms = %v", lines[i], lines[i].DurationMS)
		}
		lines[i].DurationMS = nil
	}
	slices.SortFunc(lines, func(a, b auditLine) int { return strings.Compare(a.Connection, b.Connection) })
	return text, isErr, lines
}

func toolLine(tool, conn, sql string, rows int, outcome string) auditLine {
	return auditLine{Msg: "tool call", Tool: tool, Connection: conn, SQLSHA256: sha12(sql), SQLLen: len(sql), Rows: rows, Outcome: outcome}
}

func (s *session) expectCall(t *testing.T, tool string, a map[string]any, wantErr bool, want string, wantLines ...auditLine) {
	t.Helper()
	text, isErr, lines := s.auditedCall(t, tool, a)
	if isErr != wantErr || text != want {
		t.Errorf("%s %v: error %t, result\n%s\nwant error %t,\n%s", tool, a, isErr, text, wantErr, want)
	}
	if !slices.Equal(lines, wantLines) {
		t.Errorf("%s %v: audit lines\n%+v\nwant\n%+v", tool, a, lines, wantLines)
	}
}

func TestListConnectionsShowsConfirm(t *testing.T) {
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"prod":    {Source: "direct", DSN: unusedDSN, DefaultSchema: "app", Confirm: true},
		"staging": direct(unusedDSN),
	}))
	want := "Connections (2):\n  prod [direct, schema:app, confirm]\n    role check off\n  staging [direct]\n    role check off"
	if got := s.ok(t, "pg_list_connections", nil); got != want {
		t.Errorf("pg_list_connections =\n%s\nwant\n%s", got, want)
	}
}

// Declined or cancelled, a call fails before its role check and query, on
// every protocol revision: the DSN is never dialed.
func TestConfirmDeclinedOrCancelledRunsNothing(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"prod": confirmed(unusedDSN)})
	const querySQL, explainSQL = "SELECT $1::int, $2::text", "SELECT 1"
	for _, version := range mcp.SupportedProtocolVersions() {
		for _, action := range []string{"decline", "cancel"} {
			s, u := connectUser(t, cfg, version, action)
			s.expectCall(t, "pg_query", args("prod", querySQL, "params", []any{7, "x"}), true, "declined by the user",
				toolLine("pg_query", "prod", querySQL, 0, "declined"))
			s.expectCall(t, "pg_explain", args("prod", explainSQL, "analyze", true), true, "declined by the user",
				toolLine("pg_explain", "prod", explainSQL, 0, "declined"))
			want := []string{
				"Allow pg_query to run this statement on prod?\n\nSELECT $1::int, $2::text\n\nparams: [7,\"x\"]",
				"Allow pg_explain with analyze to run this statement on prod?\n\nSELECT 1",
			}
			if got := u.questions(); !slices.Equal(got, want) {
				t.Errorf("%s, %s: questions\n%q\nwant\n%q", version, action, got, want)
			}
			for _, p := range u.forms() {
				if schema, _ := json.Marshal(p.RequestedSchema); p.Mode != "form" || string(schema) != `{"properties":{},"type":"object"}` {
					t.Errorf("%s, %s: mode %q, requested schema %s; want a form with no fields", version, action, p.Mode, schema)
				}
			}
		}
	}
}

// A client that cannot ask the user is refused before anything runs.
func TestConfirmRefusesClientsWithoutElicitation(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"prod":    confirmed(unusedDSN),
		"prod2":   confirmed(unusedDSN),
		"staging": direct(unusedDSN),
	})
	const sql = "SELECT 1"
	for _, version := range mcp.SupportedProtocolVersions() {
		s, _ := connectUser(t, cfg, version, "")
		s.expectCall(t, "pg_query", args("prod", sql), true,
			"pg_query on prod requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation",
			toolLine("pg_query", "prod", sql, 0, "error"))
		s.expectCall(t, "pg_explain", args("prod", sql, "analyze", true), true,
			"pg_explain with analyze on prod requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation",
			toolLine("pg_explain", "prod", sql, 0, "error"))
		s.expectCall(t, "pg_query", fanoutArgs([]string{"staging", "prod2", "prod"}, sql), true,
			"pg_query on prod2, prod requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation",
			toolLine("pg_query", "prod", sql, 0, "error"), toolLine("pg_query", "prod2", sql, 0, "error"),
			toolLine("pg_query", "staging", sql, 0, "error"))
	}
}

// A client that fails to ask ends the call itself: nothing runs or is audited.
func TestConfirmRunsNothingWhenTheClientFailsToAsk(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"prod": confirmed(unusedDSN)})
	for _, version := range mcp.SupportedProtocolVersions() {
		s := connectClient(t, cfg, t.TempDir(), version, &mcp.ClientOptions{
			ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				return nil, errors.New("no user at the keyboard")
			},
		})
		res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: args("prod", "SELECT 1")})
		if err == nil || !strings.Contains(err.Error(), "no user at the keyboard") {
			t.Errorf("%s: result %+v, error %v; want the client's error", version, res, err)
		}
		if lines := s.auditLines(t); len(lines) != 0 {
			t.Errorf("%s: audit lines %+v", version, lines)
		}
	}
}

// Only pg_query and pg_explain with analyze ask, once their arguments and
// the guard pass.
func TestConfirmAsksOnlyBeforeAStatementRuns(t *testing.T) {
	cfg := newConfig(config.RoleCheckOff, map[string]config.Connection{
		"prod":    confirmed(unusedDSN),
		"staging": direct(unusedDSN),
	})
	s, u := connectUser(t, cfg, "", "decline")
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pg_query", args("staging", "SELECT 1"), "connect"},
		{"pg_query", fanoutArgs([]string{"staging"}, "SELECT 1"), "staging: "},
		{"pg_explain", args("prod", "SELECT 1"), "connect"},
		{"pg_explain", args("prod", "SELECT 1", "analyze", false), "connect"},
		{"pg_query", args("prod", "DELETE FROM app.orders"), "query rejected: DeleteStmt is not allowed"},
		{"pg_query", fanoutArgs([]string{"staging", "prod"}, "DELETE FROM app.orders"), "query rejected: DeleteStmt is not allowed"},
		{"pg_explain", args("prod", "SHOW work_mem", "analyze", true), "query rejected: only a SELECT can be explained"},
		{"pg_query", args("prod", "SELECT 1", "format", "csv"), "format must be one of"},
		{"pg_query", args("prod", "SELECT 1", "verbose", true), `unknown field "verbose"`},
		{"pg_query", fanoutArgs([]string{"prod", "prod"}, "SELECT 1"), `connections lists "prod" twice`},
		{"pg_schema", map[string]any{"connection": "prod"}, "connect"},
		{"pg_search", map[string]any{"connection": "prod", "pattern": "orders"}, "connect"},
		{"pg_profile", map[string]any{"connection": "prod", "table": "app.orders"}, "connect"},
		{"pg_diff_schema", map[string]any{"left": "prod", "right": "staging"}, "connect"},
		{"pg_health", map[string]any{"connection": "prod"}, "connect"},
	} {
		if got := s.fails(t, tc.tool, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s %v = %q, want it to contain %q", tc.tool, tc.args, got, tc.want)
		}
	}
	if got := u.questions(); len(got) != 0 {
		t.Errorf("asked %q", got)
	}
	for _, line := range s.auditLines(t) {
		if line.Outcome == "declined" {
			t.Errorf("audit line %+v", line)
		}
	}
}

// A fan-out asks once about its connections that set confirm; declined, it
// runs on none.
func TestConfirmFanoutDeclinedRunsNowhere(t *testing.T) {
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"prod":    confirmed(unusedDSN),
		"prod2":   confirmed(unusedDSN),
		"staging": direct(unusedDSN),
	})
	const sql = "SELECT 1"
	for _, version := range mcp.SupportedProtocolVersions() {
		s, u := connectUser(t, cfg, version, "decline")
		s.expectCall(t, "pg_query", fanoutArgs([]string{"prod2", "staging", "prod"}, sql), true, "declined by the user",
			toolLine("pg_query", "prod", sql, 0, "declined"), toolLine("pg_query", "prod2", sql, 0, "declined"),
			toolLine("pg_query", "staging", sql, 0, "declined"))
		if got, want := u.questions(), []string{"Allow pg_query to run this statement on prod2, prod?\n\nSELECT 1"}; !slices.Equal(got, want) {
			t.Errorf("%s: questions\n%q\nwant\n%q", version, got, want)
		}
	}
}

func TestConfirmAcceptedRuns(t *testing.T) {
	t.Parallel()
	dsn := testdb.DSN(t, testdb.RoleClean, testdb.DBApp)
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"prod":    confirmed(dsn),
		"prod2":   confirmed(dsn),
		"staging": direct(dsn),
	})
	const querySQL, explainSQL, fanoutSQL = "SELECT count(*) FROM app.customers", "SELECT * FROM app.events WHERE id = 7", "SELECT 1 AS n"
	for _, version := range mcp.SupportedProtocolVersions() {
		s, u := connectUser(t, cfg, version, "accept")
		s.expectCall(t, "pg_query", args("prod", querySQL), false, "count\n50\n(1 row)", toolLine("pg_query", "prod", querySQL, 1, "ok"))
		s.expectCall(t, "pg_query", fanoutArgs([]string{"prod2", "staging", "prod"}, fanoutSQL), false,
			"connection\tn\nprod2\t1\nstaging\t1\nprod\t1\n(3 rows)",
			toolLine("pg_query", "prod", fanoutSQL, 1, "ok"), toolLine("pg_query", "prod2", fanoutSQL, 1, "ok"),
			toolLine("pg_query", "staging", fanoutSQL, 1, "ok"))
		text, isErr, lines := s.auditedCall(t, "pg_explain", args("prod", explainSQL, "analyze", true))
		if isErr || !strings.HasPrefix(text, "total cost ") || !strings.Contains(text, " ms, 1 row, ") ||
			!slices.Equal(lines, []auditLine{toolLine("pg_explain", "prod", explainSQL, 0, "ok")}) {
			t.Errorf("%s: pg_explain with analyze (error %t):\n%s\naudit lines %+v", version, isErr, text, lines)
		}
		want := []string{
			"Allow pg_query to run this statement on prod?\n\n" + querySQL,
			"Allow pg_query to run this statement on prod2, prod?\n\n" + fanoutSQL,
			"Allow pg_explain with analyze to run this statement on prod?\n\n" + explainSQL,
		}
		if got := u.questions(); !slices.Equal(got, want) {
			t.Errorf("%s: questions\n%q\nwant\n%q", version, got, want)
		}
	}
}

// The question precedes the role check: its findings start the accepted
// call's result, and role_check strict still refuses.
func TestConfirmKeepsTheRoleCheck(t *testing.T) {
	t.Parallel()
	warn := newConfig(config.RoleCheckWarn, map[string]config.Connection{"leaky": confirmed(testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp))})
	strict := newConfig(config.RoleCheckStrict, map[string]config.Connection{"super": confirmed(testdb.DSN(t, testdb.Superuser, testdb.DBApp))})
	const sql = "SELECT 1 AS n"
	for _, version := range mcp.SupportedProtocolVersions() {
		s, u := connectUser(t, warn, version, "accept")
		first := s.ok(t, "pg_query", args("leaky", sql))
		if !strings.HasPrefix(first, "role check: role ro_leaky: 3 finding(s)") || !strings.HasSuffix(first, "\n\nn\n1\n(1 row)") {
			t.Errorf("%s: first call:\n%s", version, first)
		}
		if again := s.ok(t, "pg_query", args("leaky", sql)); again != "n\n1\n(1 row)" {
			t.Errorf("%s: second call:\n%s", version, again)
		}
		if got := len(u.questions()); got != 2 {
			t.Errorf("%s: %d questions, want 2", version, got)
		}

		s, u = connectUser(t, strict, version, "accept")
		text, isErr, lines := s.auditedCall(t, "pg_query", args("super", sql))
		if !isErr || !strings.HasPrefix(text, `connection "super" refused (role_check strict): its role can do more than read.`) ||
			!slices.Equal(lines, []auditLine{toolLine("pg_query", "super", sql, 0, "preflight_block")}) || len(u.questions()) != 1 {
			t.Errorf("%s: accepted on a refused role (error %t, %d questions):\n%s\naudit lines %+v", version, isErr, len(u.questions()), text, lines)
		}
	}
}

func (s *session) queryAnswering(t *testing.T, sql string, answers mcp.InputResponseMap) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: args("prod", sql), InputResponses: answers})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", sql, err)
	}
	return res
}

func onlyQuestion(t *testing.T, res *mcp.CallToolResult) (string, *mcp.ElicitParams) {
	t.Helper()
	if !res.NeedsInput() || len(res.InputRequests) != 1 || len(res.Content) != 0 {
		t.Fatalf("result needs input %t, %d input requests, %d content items; want one question alone",
			res.NeedsInput(), len(res.InputRequests), len(res.Content))
	}
	for key, req := range res.InputRequests {
		if p, ok := req.(*mcp.ElicitParams); ok {
			return key, p
		}
	}
	t.Fatalf("input requests %v, want an elicitation", res.InputRequests)
	return "", nil
}

// From revision 2026-07-28 the client calls again with the answer, which
// holds only for the question it answers.
func TestConfirmAnswersOnlyTheQuestionAsked(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"prod": confirmed(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))})
	s := connectClient(t, cfg, t.TempDir(), "", &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			t.Error("the client answered a question itself: multi round-trip handling is off")
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	const sql, otherSQL = "SELECT 1 AS n", "SELECT 2 AS n"

	key, first := onlyQuestion(t, s.queryAnswering(t, sql, nil))
	if first.Mode != "form" || first.Message != "Allow pg_query to run this statement on prod?\n\n"+sql {
		t.Errorf("question: mode %q, message %q", first.Mode, first.Message)
	}
	otherKey, other := onlyQuestion(t, s.queryAnswering(t, otherSQL, mcp.InputResponseMap{key: &mcp.ElicitResult{Action: "accept"}}))
	if otherKey == key || other.Message != "Allow pg_query to run this statement on prod?\n\n"+otherSQL {
		t.Errorf("another statement with the first one's answer: asked %q as %q", other.Message, otherKey)
	}
	if lines := s.auditLines(t); len(lines) != 0 {
		t.Errorf("questions wrote audit lines: %+v", lines)
	}

	declined := s.queryAnswering(t, sql, mcp.InputResponseMap{key: &mcp.ElicitResult{Action: "decline"}})
	if text := resultText(t, declined); !declined.IsError || text != "declined by the user" {
		t.Errorf("declined: error %t, %q", declined.IsError, text)
	}
	accepted := s.queryAnswering(t, sql, mcp.InputResponseMap{key: &mcp.ElicitResult{Action: "accept"}})
	if text := resultText(t, accepted); accepted.IsError || text != "n\n1\n(1 row)" {
		t.Errorf("accepted: error %t, %q", accepted.IsError, text)
	}
	want := []auditLine{toolLine("pg_query", "prod", sql, 0, "declined"), toolLine("pg_query", "prod", sql, 1, "ok")}
	lines := s.auditLines(t)
	for i := range lines {
		lines[i].DurationMS = nil
	}
	if !slices.Equal(lines, want) {
		t.Errorf("audit lines\n%+v\nwant\n%+v", lines, want)
	}
}
