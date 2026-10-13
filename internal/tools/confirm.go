package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

func canAsk(caps *mcp.ClientCapabilities) bool {
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

func (h *Handlers) confirm(c *call, what string, conns []string, sql string, params []any) error {
	var asked []string
	for _, conn := range conns {
		if h.cfg.Connections[conn].Confirm {
			asked = append(asked, conn)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	on := strings.Join(asked, ", ")
	if !c.canAsk {
		return fmt.Errorf("%s on %s requires the user's confirmation, but this client cannot ask the user: it does not support form elicitation", what, on)
	}
	message, err := question(what, on, sql, params)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(message))
	key := "confirm-" + hex.EncodeToString(sum[:8])
	answer, ok := c.answers[key]
	if !ok {
		form := map[string]any{"type": "object", "properties": map[string]any{}}
		return &confirmRequest{key: key, question: &mcp.ElicitParams{Mode: "form", Message: message, RequestedSchema: form}}
	}
	if r, ok := answer.(*mcp.ElicitResult); ok && r.Action == "accept" {
		return nil
	}
	return errDeclined
}

func question(what, on, sql string, params []any) (string, error) {
	text := fmt.Sprintf("Allow %s to run this statement on %s?\n\n%s", what, on, sql)
	if len(params) == 0 {
		return text, nil
	}
	var values strings.Builder
	enc := json.NewEncoder(&values)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(params); err != nil {
		return "", err
	}
	return text + "\n\nparams: " + strings.TrimSuffix(values.String(), "\n"), nil
}
