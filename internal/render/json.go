package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jpgomesf/pgglass/internal/result"
)

// jsonNumber matches the JSON number grammar (RFC 8259, section 6).
var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

type jsonColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type jsonResult struct {
	Columns []jsonColumn `json:"columns"`
	Rows    [][]any      `json:"rows"`
}

func renderJSON(res *result.Result, maxCell int) (string, error) {
	out := jsonResult{
		Columns: make([]jsonColumn, len(res.Columns)),
		Rows:    make([][]any, len(res.Rows)),
	}
	for i, c := range res.Columns {
		out.Columns[i] = jsonColumn{Name: c.Name, Type: c.TypeName}
	}
	for i, row := range res.Rows {
		cells := make([]any, len(row))
		for j, v := range row {
			cells[j] = jsonValue(v, res.Columns[j].TypeOID, maxCell)
		}
		out.Rows[i] = cells
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	return b.String(), nil
}

// jsonValue returns the value encoding/json writes for a cell of type oid.
// A value that does not have the form its type promises (NaN, a cut value)
// falls back to a string. Embedding json and jsonb means a jsonb 'null' and
// SQL NULL both come out as null.
func jsonValue(v *string, oid uint32, maxCell int) any {
	if v == nil {
		return nil
	}
	s, cut := truncate(*v, maxCell)
	if cut {
		return s
	}
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.Float4OID, pgtype.Float8OID, pgtype.NumericOID:
		if jsonNumber.MatchString(s) {
			return json.Number(s)
		}
	case pgtype.BoolOID:
		if s == "t" || s == "f" {
			return s == "t"
		}
	case pgtype.JSONOID, pgtype.JSONBOID:
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
	}
	return s
}
