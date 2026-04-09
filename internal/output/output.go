package output

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

var tempDir = filepath.Join(os.TempDir(), "mcp-postgres")

// SmartOutput returns content inline if under the threshold, otherwise
// writes to a temp file and returns a summary.
func SmartOutput(content, toolName string, rowCount int, headers []string) string {
	if len(content) <= config.InlineMaxChars {
		return content
	}

	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		return content
	}

	ts := time.Now().UTC().Format("20060102_150405")
	filePath := filepath.Join(tempDir, fmt.Sprintf("%s_%s.txt", toolName, ts))

	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		return content
	}

	summary := fmt.Sprintf("%d rows, %d columns: %s",
		rowCount, len(headers), strings.Join(truncateSlice(headers, 10), ", "))
	if len(headers) > 10 {
		summary += fmt.Sprintf(" ... (+%d more)", len(headers)-10)
	}

	return fmt.Sprintf("%s\nSaved to: %s\n\nUse Read or Grep on the file to inspect specific data.", summary, filePath)
}

// FormatTable formats rows as an ASCII table with aligned columns.
func FormatTable(rows []map[string]string, headers []string) string {
	if len(rows) == 0 {
		return "Query returned no rows."
	}

	colWidths := make(map[string]int, len(headers))
	for _, h := range headers {
		colWidths[h] = len(h)
	}
	for _, row := range rows {
		for _, h := range headers {
			if l := len(row[h]); l > colWidths[h] {
				colWidths[h] = l
			}
		}
	}

	var sb strings.Builder

	for i, h := range headers {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(PadRight(h, colWidths[h]))
	}
	sb.WriteString("\n")

	for i, h := range headers {
		if i > 0 {
			sb.WriteString("-+-")
		}
		sb.WriteString(strings.Repeat("-", colWidths[h]))
	}
	sb.WriteString("\n")

	for _, row := range rows {
		for i, h := range headers {
			if i > 0 {
				sb.WriteString(" | ")
			}
			sb.WriteString(PadRight(row[h], colWidths[h]))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// PadRight pads a string with spaces to the given width.
func PadRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func truncateSlice(s []string, max int) []string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
