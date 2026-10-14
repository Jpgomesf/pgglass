package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/preflight"
	"github.com/jpgomesf/pgglass/internal/resolver"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const doctorSecret = "never-print-me-7f3a"

const creatorSQL = `DO $$
BEGIN
	CREATE ROLE doctor_creator LOGIN PASSWORD 'doctor_creator' CREATEDB;
EXCEPTION WHEN duplicate_object THEN NULL;
END
$$;`

func TestVerdict(t *testing.T) {
	critical := preflight.Finding{Severity: preflight.SeverityCritical, Check: "superuser", Detail: "bypasses every privilege check"}
	warning := preflight.Finding{Severity: preflight.SeverityWarning, Check: "createdb", Detail: "can create databases"}
	for _, tc := range []struct {
		report         preflight.Report
		err            error
		status, detail string
	}{
		{preflight.Report{Role: "reader"}, nil, "ok", "role reader: no findings"},
		{preflight.Report{Role: "maker", Findings: []preflight.Finding{warning}}, nil, "warn",
			"role maker: 1 finding(s)\n  warning createdb: can create databases"},
		{preflight.Report{Role: "root", Findings: []preflight.Finding{critical, warning}}, nil, "FAIL",
			"role root: 2 finding(s)\n  critical superuser: bypasses every privilege check\n  warning createdb: can create databases"},
		{preflight.Report{}, errors.New("connect: refused\nby the host"), "FAIL", "connect: refused\n  by the host"},
	} {
		if status, detail := verdict(tc.report, tc.err); status != tc.status || detail != tc.detail {
			t.Errorf("verdict(%+v, %v) = %q, %q; want %q, %q", tc.report, tc.err, status, detail, tc.status, tc.detail)
		}
	}
}

func TestSummarizeListsEachCheckThenCountsTheFailures(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"defaults": {"role_check": "strict", "readonly": true}}`))
	if err != nil {
		t.Fatal(err)
	}
	report, failed := summarize("/etc/pgglass.json", cfg, []check{
		{name: "a", report: preflight.Report{Role: "reader"}},
		{name: "b", err: errors.New("connect: refused")},
		{name: "c", report: preflight.Report{Role: "maker", Findings: []preflight.Finding{
			{Severity: preflight.SeverityWarning, Check: "createdb", Detail: "can create databases"},
		}}},
	})
	want := "config /etc/pgglass.json (role_check strict)\n" +
		"warn config: readonly is deprecated and ignored: the server is always read-only\n" +
		"ok a: role reader: no findings\n" +
		"FAIL b: connect: refused\n" +
		"warn c: role maker: 1 finding(s)\n  warning createdb: can create databases\n" +
		"1 failed, 2 passed\n"
	if report != want || !failed {
		t.Errorf("summarize = %v,\n%s\nwant true,\n%s", failed, report, want)
	}
	if report, failed := summarize("/etc/pgglass.json", config.Config{Defaults: config.Defaults{RoleCheck: "off"}}, nil); failed ||
		report != "config /etc/pgglass.json (role_check off)\n0 failed, 0 passed\n" {
		t.Errorf("summarize of no checks = %v, %q", failed, report)
	}
}

func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// pgglass doctor reports each connection in name order, never a password:
// warnings pass; critical findings, bad credentials or unreachable hosts fail.
func TestDoctorChecksEveryConnection(t *testing.T) {
	testdb.CreateDatabase(t, "doctor", creatorSQL)
	refused := refusedAddr(t)
	dotenv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(dotenv, []byte("DATABASE_URL=postgres://ro_clean:"+doctorSecret+"@"+refused+"/app?sslmode=disable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGGLASS_DOCTOR_UNSET", "")
	path := writeConfig(t, connections(map[string]any{
		"clean":   direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp)),
		"creator": direct(testdb.DSN(t, "doctor_creator", "doctor")),
		"down":    map[string]any{"source": "dotenv", "dotenv_path": dotenv, "dsn_env_var": "DATABASE_URL"},
		"garbled": direct("postgres://ro_clean:" + doctorSecret + "@127.0.0.1:port/app"),
		"leaky":   direct(testdb.DSN(t, testdb.RoleLeaky, testdb.DBApp)),
		"unset":   map[string]any{"source": "env", "dsn_env_var": "PGGLASS_DOCTOR_UNSET"},
	}))

	code, out, errOut := runCommand(t, "doctor", "--config", path)
	want := regexp.MustCompile(`^` + regexp.QuoteMeta("config "+path+" (role_check warn)\n"+
		"ok clean: role ro_clean: no findings\n"+
		"warn creator: role doctor_creator: 1 finding(s)\n"+
		"  warning createdb: can create databases\n"+
		"FAIL down: connect: failed to connect to `user=ro_clean database=app`: ") + `.*connection refused\n` +
		regexp.QuoteMeta("FAIL garbled: credentials: the DSN does not parse (not shown: it may hold a password)\n"+
			"FAIL leaky: role ro_leaky: 3 finding(s)\n"+
			"  critical function_execute: EXECUTE on ") + `.*public\.dblink.*\n` +
		regexp.QuoteMeta("  warning role_membership: member of pg_read_server_files\n"+
			"  warning table_write: INSERT on app.audit_log\n"+
			`FAIL unset: credentials: connection "unset" (env): environment variable "PGGLASS_DOCTOR_UNSET" is not set`+"\n"+
			"4 failed, 2 passed\n") + `$`)
	if code != 1 || !want.MatchString(out) || errOut != "" {
		t.Errorf("doctor = %d, stderr %q, stdout:\n%s\nwant 1 and stdout matching\n%s", code, errOut, out, want)
	}
	if strings.Contains(out+errOut, doctorSecret) {
		t.Errorf("doctor printed a password:\n%s%s", out, errOut)
	}
}

func TestDoctorPassesWhenEveryRoleOnlyReads(t *testing.T) {
	path := writeConfig(t, map[string]any{
		"defaults":    map[string]any{"role_check": "strict"},
		"connections": map[string]any{"clean": direct(testdb.DSN(t, testdb.RoleClean, testdb.DBApp))},
	})
	code, out, errOut := runCommand(t, "--config", path, "doctor")
	want := "config " + path + " (role_check strict)\nok clean: role ro_clean: no findings\n0 failed, 1 passed\n"
	if code != 0 || out != want || errOut != "" {
		t.Errorf("doctor = %d, %q, %q; want 0 and\n%s", code, out, errOut, want)
	}
}

func TestDoctorWarnsAboutTheDeprecatedReadonlyKey(t *testing.T) {
	path := writeConfig(t, map[string]any{"defaults": map[string]any{"readonly": true}})
	code, out, _ := runCommand(t, "doctor", "--config", path)
	want := "config " + path + " (role_check warn)\n" +
		"warn config: readonly is deprecated and ignored: the server is always read-only\n0 failed, 0 passed\n"
	if code != 0 || out != want {
		t.Errorf("doctor = %d, %q; want 0 and\n%s", code, out, want)
	}
}

func TestDoctorRejectsABadConfig(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	for doc, want := range map[string]string{
		"":                                    "read config: open " + missing,
		`{"connections": `:                    "parse config.json",
		`{"defaults": {"role_chek": "warn"}}`: `unknown field "role_chek"`,
		`{"defaults": {"max_rows": -1}}`:      "defaults.max_rows must be positive, got -1",
		`{"connections": {"db": {"source": "vault"}}}`: `connection "db": unknown source "vault"`,
	} {
		path := missing
		if doc != "" {
			path = filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, out, errOut := runCommand(t, "doctor", "--config", path)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "pgglass doctor: ") || !strings.Contains(errOut, want) {
			t.Errorf("%s: doctor = %d, %q, %q; want 1 and %q on stderr", doc, code, out, errOut, want)
		}
	}
}

type stallingSource struct{}

func (stallingSource) Type() string { return "stall" }

func (stallingSource) Resolve(ctx context.Context, _ string, _ config.Connection) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(20 * time.Second):
		return "", errors.New("stalled for 20s")
	}
}

func (stallingSource) Status(config.Connection) string { return "" }

func init() { resolver.RegisterSource(stallingSource{}) }

// Neither a host that never answers nor a credential source that never returns
// holds the doctor past its timeout.
func TestDoctorGivesUpOnASilentHostOrStalledCredentials(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			_ = conn.Close()
		}
	})
	path := writeConfig(t, connections(map[string]any{
		"silent":  direct("postgres://ro_clean:" + doctorSecret + "@" + ln.Addr().String() + "/app?sslmode=disable"),
		"stalled": map[string]any{"source": "stall"},
	}))
	start := time.Now()
	code, out, _ := runCommand(t, "doctor", "--config", path)
	for _, want := range []string{"FAIL silent: connect: no answer within 5s\n", "FAIL stalled: credentials: context deadline exceeded\n"} {
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("doctor = %d, %q; want 1 and %q", code, out, want)
		}
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("doctor took %s", elapsed)
	}
}
