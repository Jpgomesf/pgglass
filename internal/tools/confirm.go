package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errDeclined = errors.New("declined by the user")

type confirmRequest struct {
	key      string
	question *mcp.ElicitParams
}

func (r *confirmRequest) Error() string { return "the user's confirmation is required" }

func (r *confirmRequest) result() *mcp.CallToolResult {
	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{r.key: r.question}}
}

func asksUser(err error) bool {
	var ask *confirmRequest
	return errors.As(err, &ask)
}

const multiRoundTripRevision = "2026-07-28"

func canAsk(req *mcp.CallToolRequest) bool {
	init := req.Session.InitializeParams()
	if init == nil || init.ProtocolVersion >= multiRoundTripRevision {
		return formElicitation(req.ClientCapabilities())
	}
	return req.Extra == nil && formElicitation(init.Capabilities)
}

func formElicitation(caps *mcp.ClientCapabilities) bool {
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

type target struct {
	conn, searchPath string
}

func (h *Handlers) confirm(c *call, what string, targets []target, sql string, params []any) error {
	var asked []target
	for _, t := range targets {
		if h.cfg.Connections[t.conn].Confirm {
			asked = append(asked, t)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	if !c.canAsk {
		return fmt.Errorf("%s on %s requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation", what, connNames(asked))
	}
	message, err := question(what, asked, sql, params)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(message))
	key := "confirm-" + hex.EncodeToString(sum[:8])
	answer, ok := c.answers[key]
	if !ok {
		form := map[string]any{"type": "object", "properties": map[string]any{}}
		return &confirmRequest{key: key, question: &mcp.ElicitParams{Mode: "form", Message: visible(message), RequestedSchema: form}}
	}
	if r, ok := answer.(*mcp.ElicitResult); ok && r.Action == "accept" {
		return nil
	}
	return errDeclined
}

func question(what string, asked []target, sql string, params []any) (string, error) {
	var notes []string
	if len(params) > 0 {
		var values strings.Builder
		enc := json.NewEncoder(&values)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(params); err != nil {
			return "", err
		}
		notes = append(notes, "params: "+strings.TrimSuffix(values.String(), "\n"))
	}
	for _, t := range asked {
		if t.searchPath != "" {
			notes = append(notes, "search_path on "+t.conn+": "+pgx.Identifier{t.searchPath}.Sanitize())
		}
	}
	text := fmt.Sprintf("Allow %s to run this statement on %s?\n\n%s", what, connNames(asked), sql)
	if len(notes) == 0 {
		return text, nil
	}
	return text + "\n\n" + strings.Join(notes, "\n"), nil
}

func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)) {
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func connNames(targets []target) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.conn
	}
	return strings.Join(names, ", ")
}
