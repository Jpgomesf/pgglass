package tools_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpgomesf/pgglass/internal/config"
)

// From revision 2026-07-28 the confirmation question comes back as the result
// of the call, which the client shows the user, not the agent. So that the
// user approves the statement that runs, it quotes the whole statement and its
// params, past the 10 KB every other result keeps to: an IN list of 3000 ids,
// or the same ids as one array param, are not cut.
func TestConfirmQuestionQuotesTheWholeStatementPastTheInlineLimit(t *testing.T) {
	t.Parallel()
	cfg := newConfig(config.RoleCheckWarn, map[string]config.Connection{"prod": confirmed(unusedDSN)})
	s := connectClient(t, cfg, t.TempDir(), "", &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	ids := make([]string, 3000)
	for i := range ids {
		ids[i] = strconv.Itoa(100_000 + i)
	}
	inList := "SELECT id FROM app.orders WHERE id IN (" + strings.Join(ids, ", ") + ")"
	const anyID = "SELECT id FROM app.orders WHERE id = ANY($1::bigint[])"
	array := "{" + strings.Join(ids, ",") + "}"
	const ask = "Allow pg_query to run this statement on prod?\n\n"
	for _, tc := range []struct {
		args     map[string]any
		question string
	}{
		{args("prod", inList), ask + inList},
		{args("prod", anyID, "params", []any{array}), ask + anyID + "\n\nparams: [\"" + array + "\"]"},
	} {
		res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "pg_query", Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}
		_, question := onlyQuestion(t, res)
		if question.Message != tc.question || len(question.Message) <= config.InlineMaxBytes {
			t.Errorf("confirmation question of %d bytes, want the whole %d-byte question, past %d:\n%.200s",
				len(question.Message), len(tc.question), config.InlineMaxBytes, question.Message)
		}
	}
}
