package guard

import (
	"errors"
	"strings"
	"testing"

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
