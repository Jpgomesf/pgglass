package introspect_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const generatedSQL = `CREATE TABLE prices (id int PRIMARY KEY, net numeric NOT NULL);
INSERT INTO prices SELECT g, g FROM generate_series(1, 100) g;
DO $$
BEGIN
    EXECUTE format('ALTER TABLE prices ADD COLUMN gross numeric GENERATED ALWAYS AS (net * 2) %s',
        CASE WHEN current_setting('server_version_num')::int >= 180000 THEN 'VIRTUAL' ELSE 'STORED' END);
END $$;
ANALYZE prices;`

// From PostgreSQL 18, gross is a virtual generated column, shown as VIRTUAL;
// ANALYZE keeps no statistics on it, so pg_profile samples it alone.
func TestVirtualGeneratedColumns(t *testing.T) {
	const name = "introspect_generated"
	testdb.CreateDatabase(t, name, generatedSQL)
	q := testdb.Querier(t, testdb.Superuser, name)
	virtual := testdb.ServerVersion(t) >= 180000
	generated := "GENERATED ALWAYS AS (net * 2::numeric) STORED"
	if virtual {
		generated = "GENERATED ALWAYS AS (net * 2::numeric) VIRTUAL"
	}

	equal(t, "columns", describe(t, q, "public", "prices").Columns, []introspect.Column{
		{Name: "id", Type: "integer", NotNull: true},
		{Name: "net", Type: "numeric", NotNull: true},
		{Name: "gross", Type: "numeric", Generated: generated},
	})
	s, err := introspect.Structure(t.Context(), q, "public")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "structure columns", s["prices"].Columns, map[string]string{
		"id": "integer NOT NULL", "net": "numeric NOT NULL", "gross": "numeric " + generated,
	})

	all := profile(t, q, "public", "prices", "")
	if gross := columns(all)["gross"]; all.Sampled || (gross.Stats == nil) != virtual {
		t.Errorf("profile of prices: sampled %v, gross %+v", all.Sampled, gross)
	}
	alone := profile(t, q, "public", "prices", "gross")
	if alone.Sampled != virtual || (virtual && (alone.Sample != 100 || alone.Columns[0].Distinct != 100)) {
		t.Errorf("profile of gross alone = %+v", alone)
	}
	if want := "virtual generated columns, which ANALYZE skips)"; virtual && !strings.HasSuffix(alone.Summary(), want) {
		t.Errorf("summary %q does not end with %q", alone.Summary(), want)
	}
}
