package tools

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

func TestCanAskNeedsFormElicitation(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps *mcp.ClientCapabilities
		want bool
	}{
		{"no capabilities", nil, false},
		{"no elicitation", &mcp.ClientCapabilities{}, false},
		{"elicitation before modes", &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{}}, true},
		{"form", &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}, true},
		{"url alone", &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}}, false},
		{"form and url", &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{},
		}}, true},
	} {
		if got := canAsk(tc.caps); got != tc.want {
			t.Errorf("%s: canAsk = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestQuestionShowsTheStatementAndItsParams(t *testing.T) {
	for _, tc := range []struct {
		params []any
		want   string
	}{
		{nil, "Allow pg_query to run this statement on prod, prod2?\n\nSELECT $1"},
		{[]any{json.Number("12345678901234567890.5"), "a<b&c>", nil, true}, "Allow pg_query to run this statement on prod, prod2?\n\nSELECT $1\n\n" +
			`params: [12345678901234567890.5,"a<b&c>",null,true]`},
	} {
		got, err := question("pg_query", "prod, prod2", "SELECT $1", tc.params)
		if err != nil || got != tc.want {
			t.Errorf("question with params %v = %q, %v; want %q", tc.params, got, err, tc.want)
		}
	}
	if _, err := question("pg_query", "prod", "SELECT $1", []any{json.Number("not a number")}); err == nil {
		t.Error("a param JSON cannot encode: no error")
	}
}

func confirmHandlers() *Handlers {
	return &Handlers{cfg: config.Config{Connections: map[string]config.Connection{
		"prod":    {Source: "direct", Confirm: true},
		"prod2":   {Source: "direct", Confirm: true},
		"staging": {Source: "direct"},
	}}}
}

func TestConfirmGoesOnWhenNoConnectionSetsConfirm(t *testing.T) {
	h := confirmHandlers()
	for _, c := range []*call{{}, {canAsk: true}} {
		if err := h.confirm(c, "pg_query", []string{"staging"}, "SELECT 1", nil); err != nil {
			t.Errorf("can ask %t: %v", c.canAsk, err)
		}
	}
}

func TestConfirmRefusesAClientThatCannotAsk(t *testing.T) {
	err := confirmHandlers().confirm(&call{}, "pg_query", []string{"prod", "staging", "prod2"}, "SELECT 1", nil)
	want := "pg_query on prod, prod2 requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation"
	if err == nil || err.Error() != want || asksUser(err) {
		t.Errorf("confirm = %v, want %q", err, want)
	}
}

func askFor(t *testing.T, h *Handlers, conns []string, sql string) *confirmRequest {
	t.Helper()
	err := h.confirm(&call{canAsk: true}, "pg_query", conns, sql, nil)
	var ask *confirmRequest
	if !errors.As(err, &ask) || !asksUser(err) {
		t.Fatalf("confirm = %v, want a question", err)
	}
	return ask
}

func TestConfirmAsksWhenTheCallBringsNoAnswer(t *testing.T) {
	ask := askFor(t, confirmHandlers(), []string{"staging", "prod"}, "SELECT 1")
	q := ask.question
	if q.Mode != "form" || q.Message != "Allow pg_query to run this statement on prod?\n\nSELECT 1" {
		t.Errorf("question: mode %q, message %q", q.Mode, q.Message)
	}
	if schema, _ := json.Marshal(q.RequestedSchema); string(schema) != `{"properties":{},"type":"object"}` {
		t.Errorf("requested schema %s", schema)
	}
	res := ask.result()
	if len(res.Content) != 0 || res.IsError || len(res.InputRequests) != 1 || res.InputRequests[ask.key] != q {
		t.Errorf("result %+v, want the question alone", res)
	}
	if got := ask.Error(); got != "the user's confirmation is required" {
		t.Errorf("Error() = %q", got)
	}
}

// A statement whose params the question cannot show is refused, never asked
// about or run unconfirmed.
func TestConfirmFailsWhenItCannotWriteTheQuestion(t *testing.T) {
	err := confirmHandlers().confirm(&call{canAsk: true}, "pg_query", []string{"prod"}, "SELECT $1", []any{json.Number("not a number")})
	if err == nil || asksUser(err) || errors.Is(err, errDeclined) {
		t.Errorf("confirm = %v, want the question's error", err)
	}
}

func TestConfirmKeysEachQuestionByWhatItAsks(t *testing.T) {
	h := confirmHandlers()
	first := askFor(t, h, []string{"prod"}, "SELECT 1").key
	if !regexp.MustCompile(`^confirm-[0-9a-f]{16}$`).MatchString(first) {
		t.Errorf("key %q", first)
	}
	if again := askFor(t, h, []string{"staging", "prod"}, "SELECT 1").key; again != first {
		t.Errorf("the same question: key %q, want %q", again, first)
	}
	for _, tc := range []struct {
		conns []string
		sql   string
	}{
		{[]string{"prod"}, "SELECT 2"},
		{[]string{"prod2"}, "SELECT 1"},
		{[]string{"prod", "prod2"}, "SELECT 1"},
	} {
		if key := askFor(t, h, tc.conns, tc.sql).key; key == first {
			t.Errorf("%s on %v: key %q, the first question's", tc.sql, tc.conns, key)
		}
	}
}

func TestConfirmTakesOnlyAnAcceptedAnswerToItsQuestion(t *testing.T) {
	h := confirmHandlers()
	ask := askFor(t, h, []string{"prod"}, "SELECT 1")
	for _, tc := range []struct {
		name   string
		answer mcp.InputResponse
		want   error
	}{
		{"accept", &mcp.ElicitResult{Action: "accept"}, nil},
		{"accept with content", &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil},
		{"decline", &mcp.ElicitResult{Action: "decline"}, errDeclined},
		{"cancel", &mcp.ElicitResult{Action: "cancel"}, errDeclined},
		{"unknown action", &mcp.ElicitResult{Action: "maybe"}, errDeclined},
		{"an empty answer", nil, errDeclined},
	} {
		c := &call{canAsk: true, answers: mcp.InputResponseMap{ask.key: tc.answer}}
		if err := h.confirm(c, "pg_query", []string{"prod"}, "SELECT 1", nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: confirm = %v, want %v", tc.name, err, tc.want)
		}
	}
	other := askFor(t, h, []string{"prod"}, "SELECT 2")
	c := &call{canAsk: true, answers: mcp.InputResponseMap{other.key: &mcp.ElicitResult{Action: "accept"}}}
	if err := h.confirm(c, "pg_query", []string{"prod"}, "SELECT 1", nil); !asksUser(err) {
		t.Errorf("an answer to another question: confirm = %v, want the question asked", err)
	}
}

func TestDeclinedCallsAreAuditedAsDeclined(t *testing.T) {
	if outcome, sqlState := classify(errDeclined); outcome != "declined" || sqlState != "" {
		t.Errorf("classify = %s %q, want declined", outcome, sqlState)
	}
	if got := errorText(errDeclined); got != "declined by the user" {
		t.Errorf("errorText = %q", got)
	}
}
