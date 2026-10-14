package guard

import (
	"errors"
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"github.com/wasilibs/go-pgquery/parser"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestWalkRejectsMapFields(t *testing.T) {
	msg, err := structpb.NewStruct(map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	err = walk(msg.ProtoReflect())
	var v *Violation
	if !errors.As(err, &v) || !strings.Contains(v.Reason, "cannot check") {
		t.Errorf("walk(message with a map field) = %v, want a *Violation", err)
	}
}

func TestIsDeniedRejectsAnEmptyName(t *testing.T) {
	if !isDenied(nil) {
		t.Error("isDenied(nil) = false, want true")
	}
}

func TestNodeNameOfNoNode(t *testing.T) {
	if got := nodeName(nil); got != "nothing" {
		t.Errorf("nodeName(nil) = %q, want %q", got, "nothing")
	}
}

func TestSafelyTurnsPanicsIntoErrors(t *testing.T) {
	_, err := safely(func(string) (int, error) { panic("stack overflow") }, "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "parser failed: stack overflow") {
		t.Errorf("safely(panicking func) error = %v", err)
	}
	got, err := safely(func(s string) (int, error) { return len(s), nil }, "SELECT 1")
	if got != 8 || err != nil {
		t.Errorf("safely(len) = %d, %v; want 8, nil", got, err)
	}
}

// The grammar nests statements in a query only under WITH, which checkCTE
// handles; walk still rejects one found anywhere else, and allows a SELECT.
func TestWalkRejectsAStatementNestedInAQuery(t *testing.T) {
	where := func(n *pg_query.Node) error {
		return walk((&pg_query.SelectStmt{WhereClause: n}).ProtoReflect())
	}
	deletion := &pg_query.Node{Node: &pg_query.Node_DeleteStmt{DeleteStmt: &pg_query.DeleteStmt{}}}
	var v *Violation
	if err := where(deletion); !errors.As(err, &v) || v.Reason != "DeleteStmt is not allowed inside a query" {
		t.Errorf("walk(SELECT with a DeleteStmt in WHERE) = %v, want a *Violation naming DeleteStmt", err)
	}
	subquery := &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: &pg_query.SelectStmt{}}}
	if err := where(subquery); err != nil {
		t.Errorf("walk(SELECT with a SelectStmt in WHERE) = %v, want nil", err)
	}
}

// A parser error names the character it points at, when it points at one.
func TestParseErrorText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&parser.Error{Message: `syntax error at or near "FORM"`, Cursorpos: 10}, `syntax error at or near "FORM" at character 10`},
		{&parser.Error{Message: "stack depth limit exceeded"}, "stack depth limit exceeded"},
		{errors.New("parser failed: out of memory"), "parser failed: out of memory"},
	} {
		if got := parseErrorText(tc.err); got != tc.want {
			t.Errorf("parseErrorText(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
