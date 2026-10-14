package render

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		s     string
		limit int
		want  string
		cut   bool
	}{
		{"hello", 0, "hello", false},
		{"hello", -1, "hello", false},
		{"hello", 5, "hello", false},
		{"hello", 6, "hello", false},
		{"hello!", 5, "hello…[+1 chars]", true},
		{"", 3, "", false},
		{"日本語", 3, "日本語", false},
		{"日本語です", 4, "日本語で…[+1 chars]", true},
		{"aé☕b", 2, "aé…[+2 chars]", true},
		{strings.Repeat("x", 250), 200, strings.Repeat("x", 200) + "…[+50 chars]", true},
	}
	for _, tt := range tests {
		got, cut := truncate(tt.s, tt.limit)
		if got != tt.want || cut != tt.cut {
			t.Errorf("truncate(%q, %d) = %q, %t; want %q, %t", tt.s, tt.limit, got, cut, tt.want, tt.cut)
		}
	}
}

func FuzzTruncate(f *testing.F) {
	for _, s := range []string{"", "a", "héllo", "日本語", "\xff\xfe\xfd", "e\u0301e\u0301"} {
		f.Add(s, 1)
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, limit int) {
		if limit < 1 || limit > 100 {
			t.Skip()
		}
		got, cut := truncate(s, limit)
		total := utf8.RuneCountInString(s)
		if total <= limit {
			if cut || got != s {
				t.Fatalf("truncate(%q, %d) = %q, %t; want it unchanged", s, limit, got, cut)
			}
			return
		}
		prefix, ok := strings.CutSuffix(got, "…[+"+strconv.Itoa(total-limit)+" chars]")
		if !cut || !ok || !strings.HasPrefix(s, prefix) || utf8.RuneCountInString(prefix) != limit {
			t.Fatalf("truncate(%q, %d) = %q, %t; want its first %d characters and a marker", s, limit, got, cut, limit)
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) split a character: %q", s, limit, got)
		}
	})
}
