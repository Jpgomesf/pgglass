package engine_test

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/engine"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const pgbouncerImage = "edoburu/pgbouncer:v1.25.2-p0"

const pgbouncerINI = `[databases]
* = host=pg port=5432

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 5432
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
default_pool_size = 4
max_client_conn = 200
track_extra_parameters = IntervalStyle, default_transaction_read_only
`

const bouncerFixture = `
CREATE SCHEMA s1;
CREATE TABLE s1.t (v text);
INSERT INTO s1.t VALUES ('s1');
CREATE SCHEMA s2;
CREATE TABLE s2.t (v text);
INSERT INTO s2.t VALUES ('s2');
CREATE TYPE mood AS ENUM ('ok', 'meh');
`

// In transaction mode, PgBouncer hands each transaction to any server
// connection, so settings and statements must not outlive it.
func TestThroughPgBouncerInTransactionMode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
	t.Parallel()
	dsn := startPgBouncer(t) + "&pool_max_conns=50"
	m := newManager(t, config.Defaults{MaxRows: 5}, dsn)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			schema := "s" + strconv.Itoa(1+i%2)
			res, err := m.Query(t.Context(), db, engine.Query{
				SQL:        "SELECT v, $1::int, pg_sleep(0.02) FROM t",
				Params:     []any{i},
				SearchPath: schema,
			})
			if err != nil {
				t.Errorf("query %d: %v", i, err)
				return
			}
			if got := deref(res.Rows[0][0]) + " " + deref(res.Rows[0][1]); got != schema+" "+strconv.Itoa(i) {
				t.Errorf("query %d in %s got %s", i, schema, got)
			}
		}()
	}
	wg.Wait()

	if got := value(t, m, "SELECT current_setting('default_transaction_read_only')"); got != "on" {
		t.Errorf("default_transaction_read_only through PgBouncer = %s, want on", got)
	}
	res := mustQuery(t, m, engine.Query{SQL: "SELECT 'ok'::mood"})
	if c := res.Columns[0]; c.TypeName != "mood" {
		t.Errorf("enum column = %+v, want type mood", c)
	}

	res = mustQuery(t, m, engine.Query{SQL: "SELECT g FROM generate_series(1, 1000000) g"})
	if len(res.Rows) != 5 || res.TruncatedReason != "max_rows=5" {
		t.Errorf("%d rows, truncated %q; want 5, max_rows=5", len(res.Rows), res.TruncatedReason)
	}

	_, err := m.Query(t.Context(), db, engine.Query{SQL: "INSERT INTO s1.t VALUES ('written')"})
	wantSQLState(t, err, "25006")
	if got := value(t, m, "SELECT count(*) FROM s1.t"); got != "1" {
		t.Errorf("s1.t has %s rows, want 1", got)
	}
}

func startPgBouncer(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	testcontainers.CleanupNetwork(t, nw)

	pg, err := postgres.Run(ctx, testdb.Image(),
		postgres.WithDatabase("app"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies(),
		network.WithNetwork([]string{"pg"}, nw),
	)
	testcontainers.CleanupContainer(t, pg)
	if err != nil {
		t.Fatalf("start Postgres: %v", err)
	}
	loadFixture(t, pg, bouncerFixture)

	bouncer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        pgbouncerImage,
			ExposedPorts: []string{"5432/tcp"},
			Networks:     []string{nw.Name},
			Files: []testcontainers.ContainerFile{
				configFile("/etc/pgbouncer/pgbouncer.ini", pgbouncerINI),
				configFile("/etc/pgbouncer/userlist.txt", `"postgres" "postgres"`+"\n"),
			},
			WaitingFor: wait.ForListeningPort("5432/tcp"),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, bouncer)
	if err != nil {
		t.Fatalf("start PgBouncer: %v", err)
	}
	host, err := bouncer.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := bouncer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", "postgres"),
		Host:     net.JoinHostPort(host, port.Port()),
		Path:     "/app",
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func configFile(path, content string) testcontainers.ContainerFile {
	return testcontainers.ContainerFile{Reader: strings.NewReader(content), ContainerFilePath: path, FileMode: 0o644}
}

func loadFixture(t *testing.T, pg *postgres.PostgresContainer, sql string) {
	t.Helper()
	dsn, err := pg.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
}
