package result

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestTextHoldsTextColumnsAndCopiedRows(t *testing.T) {
	res := Text("section", "detail")
	want := []Column{
		{Name: "section", TypeOID: pgtype.TextOID, TypeName: "text"},
		{Name: "detail", TypeOID: pgtype.TextOID, TypeName: "text"},
	}
	if !slices.Equal(res.Columns, want) || res.Rows != nil || res.Truncated {
		t.Fatalf("Text = %+v, want columns %+v and no rows", res, want)
	}

	cells := []string{"a", "b"}
	res.AddRow(cells...)
	cells[0] = "changed" // the row holds copies
	res.AddRow("c", "")
	var got [][]string
	for _, row := range res.Rows {
		var values []string
		for _, v := range row {
			if v == nil {
				t.Fatal("AddRow added a NULL")
			}
			values = append(values, *v)
		}
		got = append(got, values)
	}
	if !slices.EqualFunc(got, [][]string{{"a", "b"}, {"c", ""}}, slices.Equal) {
		t.Errorf("rows = %q, want [[a b] [c ]]", got)
	}
}
