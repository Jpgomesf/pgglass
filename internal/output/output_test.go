package output

import (
	"testing"
)

func TestFormatTable(t *testing.T) {
	tests := []struct {
		name    string
		rows    []map[string]string
		headers []string
		want    string
	}{
		{
			name:    "empty rows",
			rows:    nil,
			headers: []string{"a"},
			want:    "Query returned no rows.",
		},
		{
			name: "single row",
			rows: []map[string]string{
				{"id": "1", "name": "alice"},
			},
			headers: []string{"id", "name"},
			want:    "id | name \n---+------\n1  | alice\n",
		},
		{
			name: "multiple rows with alignment",
			rows: []map[string]string{
				{"id": "1", "name": "alice"},
				{"id": "42", "name": "bob"},
			},
			headers: []string{"id", "name"},
			want:    "id | name \n---+------\n1  | alice\n42 | bob  \n",
		},
		{
			name: "value wider than header",
			rows: []map[string]string{
				{"x": "longvalue"},
			},
			headers: []string{"x"},
			want:    "x        \n---------\nlongvalue\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatTable(tt.rows, tt.headers)
			if got != tt.want {
				t.Errorf("FormatTable() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestPadRight(t *testing.T) {
	tests := []struct {
		s     string
		width int
		want  string
	}{
		{"hi", 5, "hi   "},
		{"hello", 5, "hello"},
		{"toolong", 3, "toolong"},
		{"", 4, "    "},
	}

	for _, tt := range tests {
		got := PadRight(tt.s, tt.width)
		if got != tt.want {
			t.Errorf("PadRight(%q, %d) = %q, want %q", tt.s, tt.width, got, tt.want)
		}
	}
}

func TestTruncateSlice(t *testing.T) {
	tests := []struct {
		name string
		s    []string
		max  int
		want int
	}{
		{"under limit", []string{"a", "b"}, 5, 2},
		{"at limit", []string{"a", "b", "c"}, 3, 3},
		{"over limit", []string{"a", "b", "c", "d"}, 2, 2},
		{"empty", nil, 5, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateSlice(tt.s, tt.max)
			if len(got) != tt.want {
				t.Errorf("truncateSlice() len = %d, want %d", len(got), tt.want)
			}
		})
	}
}
