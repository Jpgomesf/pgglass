package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A refused request gets its 401 or 403 at once and its connection closes,
// even when it declares a body it never sends.
func TestHTTPRefusesRequestsBeforeTheirBody(t *testing.T) {
	isolateHome(t)
	t.Setenv(authEnv, testToken)
	srv := startHTTP(t, writeConfig(t, connections(map[string]any{})))
	host := strings.TrimSuffix(strings.TrimPrefix(srv.url, "http://"), mcpPath)
	cases := map[string]struct {
		headers string
		status  int
	}{
		"no token, length":  {"Content-Length: 100\r\n", http.StatusUnauthorized},
		"no token, chunked": {"Transfer-Encoding: chunked\r\n", http.StatusUnauthorized},
		"wrong token":       {"Authorization: Bearer " + testToken[1:] + "\r\nContent-Length: 100\r\n", http.StatusUnauthorized},
		"foreign origin":    {"Origin: http://evil.example\r\nContent-Length: 100\r\n", http.StatusForbidden},
	}
	conns := map[string]net.Conn{}
	for name, tc := range cases {
		conn, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n%s\r\n", host, tc.headers); err != nil {
			t.Fatal(err)
		}
		conns[name] = conn
	}
	for name, conn := range conns {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(conn)
		res, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Errorf("%s: no response within 5s to a request whose body is still to come: %v", name, err)
			continue
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != cases[name].status {
			t.Errorf("%s: status %d, want %d", name, res.StatusCode, cases[name].status)
		}
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout + 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		var timeout net.Error
		if _, err := r.ReadByte(); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
			t.Errorf("%s: the connection is still open after the refusal: %v", name, err)
		}
	}
}
