// Package render turns a result.Result into text for tool output.
package render

import (
	"errors"

	"github.com/jpgomesf/mcp-postgres/internal/result"
)

var errNotImplemented = errors.New("not implemented")

// Format is an output format. Its values match config.Format* for the
// formats config accepts; CSV is for spill files.
type Format string

// Formats.
const (
	TSV      Format = "tsv"
	JSON     Format = "json"
	Markdown Format = "markdown"
	CSV      Format = "csv"
)

// Render renders the header and rows of res in format f, and nothing else:
// no row counts or truncation notes. NULL renders distinctly from the empty
// string. maxCell > 0 truncates longer cell values to maxCell characters;
// 0 disables cell truncation. An unknown format is an error.
func Render(res *result.Result, f Format, maxCell int) (string, error) {
	return "", errNotImplemented
}
