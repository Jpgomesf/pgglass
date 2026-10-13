package health

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
	"github.com/jpgomesf/mcp-postgres/internal/result"
)

var (
	clean = viewer{Role: "ro_clean"}
	super = viewer{Role: "postgres", AllStats: true}
)

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %#v\nwant %#v", got, want)
	}
}

func ptr[T any](v T) *T { return &v }

func TestConnectionLines(t *testing.T) {
	c := connections{Max: 100, Used: 9, States: []stateCount{{"idle", 5}, {"active", 2}, {"", 2}}}
	equal(t, connectionLines(c, super), []string{"9 of max_connections 100: idle 5, active 2, no state 2"})
	equal(t, connectionLines(c, clean),
		[]string{"9 of max_connections 100: idle 5, active 2; the state of 2 is not visible to role ro_clean"})
	equal(t, connectionLines(connections{Max: 100, Used: 3, States: []stateCount{{"", 3}}}, clean),
		[]string{"3 of max_connections 100; the state of 3 is not visible to role ro_clean"})
}

func TestSessionLines(t *testing.T) {
	blocked := list[session]{Total: 12, Items: []session{
		{PID: 76, Database: "app", State: "active", Seconds: 65, Wait: "Lock:transactionid", App: "psql", BlockedBy: []int{75, 74}},
		{PID: 80, Database: "app", App: `say "hi"`, BlockedBy: []int{76}}, // another role's session
	}}
	equal(t, blockedLines(blocked, clean), []string{
		`pid 76, database app, active for 1m5s, waiting on Lock:transactionid, application "psql", blocked by 75 74`,
		`pid 80, database app, state not visible to role ro_clean, application "say \"hi\"", blocked by 76`,
		"… and 10 more",
	})
	equal(t, blockedLines(list[session]{}, clean), []string{"none"})

	// A session idle in transaction is as old as its transaction; one whose
	// transaction failed, as its last query.
	long := list[session]{Total: 2, Items: []session{
		{PID: 75, Database: "app", State: "idle in transaction", Seconds: 3700, Wait: "Client:ClientRead"},
		{PID: 76, Database: "app", State: "idle in transaction (aborted)", Seconds: 65, Wait: "Client:ClientRead"},
	}}
	equal(t, longRunningLines(long, super), []string{
		"pid 75, database app, idle in transaction, transaction open for 1h1m40s, waiting on Client:ClientRead",
		"pid 76, database app, idle in transaction (aborted) for 1m5s, waiting on Client:ClientRead",
	})
	equal(t, longRunningLines(long, clean), []string{
		"pid 75, database app, idle in transaction, transaction open for 1h1m40s, waiting on Client:ClientRead",
		"pid 76, database app, idle in transaction (aborted) for 1m5s, waiting on Client:ClientRead",
		"other roles' sessions are not visible to role ro_clean",
	})
	equal(t, longRunningLines(list[session]{}, super), []string{"none"})
}

func TestStatisticsLines(t *testing.T) {
	equal(t, cacheHitLines(cacheHit{Database: "app", Hit: 997, Read: 3}, clean), []string{"99.70% in database app"})
	equal(t, cacheHitLines(cacheHit{Database: "app"}, clean), []string{"no blocks read yet in database app"})

	equal(t, deadTupleLines(nil, clean), []string{"none"})
	equal(t, deadTupleLines([]table{
		{Name: "app.orders", Dead: 25, Live: 75},
		{Name: `app."Order Lines"`, Dead: 10, Live: 1000, AutovacuumSecondsAgo: ptr[int64](7200)},
	}, clean), []string{
		"app.orders: 25.0% dead (25 dead, 75 live), never autovacuumed",
		`app."Order Lines": 1.0% dead (10 dead, 1000 live), autovacuumed 2h0m0s ago`,
	})

	equal(t, unusedIndexLines(list[index]{Total: 11, Items: []index{{Name: "app.orders_customer_id_idx", Table: "app.orders", Size: "16 kB"}}}, clean),
		[]string{"app.orders_customer_id_idx on app.orders: 16 kB", "… and 10 more"})
	equal(t, unusedIndexLines(list[index]{}, clean), []string{"none"})

	equal(t, wraparoundLines(wraparound{Database: "app", Age: 150_000_000, FreezeMaxAge: 200_000_000}, clean),
		[]string{"oldest age(datfrozenxid) 150000000 (database app): 75.0% of autovacuum_freeze_max_age 200000000"})

	equal(t, replicationLines(replication{}, clean), []string{"not a replica"})
	equal(t, replicationLines(replication{Replica: true}, clean), []string{"replica: no transaction replayed yet"})
	equal(t, replicationLines(replication{Replica: true, ReplayedSecondsAgo: ptr[int64](3), UnreplayedBytes: ptr[int64](8192)}, clean),
		[]string{"replica: last transaction replayed 3s ago, 8192 bytes of received WAL not replayed yet"})
}

// fakeQuerier returns one value, or an error, and records the queries it ran.
type fakeQuerier struct {
	value     *string
	truncated bool
	err       error
	got       []engine.Query
}

func (f *fakeQuerier) Query(_ context.Context, q engine.Query) (*result.Result, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	return &result.Result{Columns: []result.Column{{Name: "json"}}, Rows: [][]*string{{f.value}}, Truncated: f.truncated}, nil
}

// A section the role may not read, or whose query fails in Postgres, says so
// in its line; anything else fails the check.
func TestSectionRun(t *testing.T) {
	s := section{name: "cache_hit", sql: cacheHitSQL, render: decoded(cacheHitLines)}
	for _, tc := range []struct {
		q    *fakeQuerier
		want []string
	}{
		{&fakeQuerier{value: ptr(`{"Database": "app", "Hit": 1, "Read": 1}`)}, []string{"50.00% in database app"}},
		{&fakeQuerier{err: &engine.QueryError{SQLState: "42501", Message: "permission denied for view pg_stat_database"}},
			[]string{"not visible to role ro_clean"}},
		{&fakeQuerier{err: &engine.QueryError{SQLState: "57014", Message: "canceling statement due to statement timeout"}},
			[]string{"failed: canceling statement due to statement timeout (SQLSTATE 57014)"}},
	} {
		got, err := s.run(context.Background(), tc.q, clean)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, got, tc.want)
		if q := tc.q.got[0]; q.SQL != cacheHitSQL || q.SearchPath != "pg_catalog" {
			t.Errorf("ran %+v, want cacheHitSQL with search_path pg_catalog", q)
		}
	}

	boom := errors.New("dial tcp: connection refused")
	for _, q := range []*fakeQuerier{
		{err: boom},
		{value: ptr(`{"Database": "app", "Hits": 1}`)}, // a key the SQL misspells
		{value: ptr(`{}`), truncated: true},
	} {
		if got, err := s.run(context.Background(), q, clean); err == nil {
			t.Errorf("run = %q, want an error", got)
		}
	}
}

func TestQueryValueReadsNullWhenNothingComesBack(t *testing.T) {
	data, err := queryValue(context.Background(), &fakeQuerier{}, cacheHitSQL)
	if err != nil || string(data) != "null" {
		t.Errorf("queryValue of NULL = %s, %v; want null", data, err)
	}
}
