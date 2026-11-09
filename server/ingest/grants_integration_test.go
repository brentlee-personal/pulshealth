package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ingestGrants is exactly what the scoped `ingest` role may do to each table
// in public (099_read_roles.sh, "ingest"): what store.go and devices.go use,
// and the `ingest devices` CLI, which runs as this role too. Every other
// table — schema_migrations, category_labels, the rollup views — gets
// nothing. Keep it in step with the list there.
var ingestGrants = map[string]string{
	"users":                     "INSERT UPDATE SELECT",
	"sample_types":              "INSERT UPDATE SELECT",
	"sources":                   "INSERT SELECT",
	"temporal_contexts":         "INSERT SELECT",
	"aggregate_series":          "INSERT SELECT",
	"aggregate_samples":         "INSERT UPDATE SELECT",
	"activity_summaries":        "INSERT UPDATE SELECT",
	"quantity_samples":          "DELETE INSERT SELECT",
	"category_samples":          "DELETE INSERT SELECT",
	"workouts":                  "DELETE INSERT SELECT",
	"heartbeat_series":          "DELETE INSERT SELECT",
	"ecg_samples":               "DELETE INSERT SELECT",
	"state_of_mind":             "DELETE INSERT SELECT",
	"medication_dose_events":    "DELETE INSERT SELECT",
	"workout_route_points":      "DELETE INSERT SELECT",
	"workout_series_points":     "DELETE INSERT SELECT",
	"deleted_samples":           "INSERT SELECT",
	"batches":                   "INSERT UPDATE SELECT",
	"family_batch_receipts":     "INSERT UPDATE SELECT",
	"family_record_identity":    "INSERT SELECT",
	"family_source_connections": "INSERT SELECT",
	"family_device_generations": "INSERT SELECT",
	"ingest_rejections":         "INSERT",
	"device_tokens":             "INSERT UPDATE SELECT",
}

// TestIntegration_IngestRoleGrants proves, as the role production connects
// as, that the grant list is exact: the privileges it holds on every
// relation in public (PUBLIC's included) are the ones above, and the writes
// it has no business making are refused. The rest of this package's
// integration tests prove the list is enough — CI runs them all as this
// role. Skipped when DATABASE_URL is some other role (a superuser locally).
func TestIntegration_IngestRoleGrants(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var role string
	if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	if role != "ingest" {
		t.Skipf("DATABASE_URL connects as %q, not the scoped ingest role", role)
	}

	rows, err := pool.Query(ctx, `
		SELECT c.relname::text,
		       coalesce(string_agg(p.priv, ' ' ORDER BY p.priv)
		                  FILTER (WHERE has_table_privilege(c.oid, p.priv)), '')
		FROM pg_class c
		CROSS JOIN unnest(ARRAY['SELECT', 'INSERT', 'UPDATE', 'DELETE',
		                        'TRUNCATE', 'REFERENCES', 'TRIGGER']) AS p(priv)
		WHERE c.relnamespace = 'public'::regnamespace
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		GROUP BY c.relname`)
	if err != nil {
		t.Fatalf("read privileges: %v", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var rel, privs string
		if err := rows.Scan(&rel, &privs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rel] = privs
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("privileges: %v", err)
	}
	want := func(rel string) string { // "" for a table off the list
		fields := strings.Fields(ingestGrants[rel])
		sort.Strings(fields)
		return strings.Join(fields, " ")
	}
	for rel := range ingestGrants {
		if _, ok := got[rel]; !ok {
			t.Errorf("table %s on the grant list does not exist", rel)
		}
	}
	for rel, privs := range got {
		if privs != want(rel) {
			t.Errorf("ingest on %s: %q, want %q", rel, privs, want(rel))
		}
	}

	// And the statements themselves are refused, not merely unlisted.
	for _, stmt := range []string{
		`DELETE FROM device_tokens WHERE false`,
		`DELETE FROM users WHERE false`,
		`DELETE FROM batches WHERE false`,
		`SELECT 1 FROM ingest_rejections LIMIT 1`,
		`SELECT 1 FROM schema_migrations LIMIT 1`,
		`INSERT INTO schema_migrations (filename) VALUES ('999_never.sql')`,
		`SELECT 1 FROM category_labels LIMIT 1`,
		`SELECT 1 FROM metric_daily LIMIT 1`,
		`UPDATE quantity_samples SET value = value WHERE false`,
		`UPDATE family_record_identity SET payload_digest = payload_digest WHERE false`,
		`DELETE FROM family_record_identity WHERE false`,
		`UPDATE family_source_connections SET status = status WHERE false`,
		`UPDATE family_device_generations SET device_generation = device_generation WHERE false`,
		`SELECT bind_personal_device_source()`,
		`TRUNCATE batches`,
		`CREATE TABLE public.ingest_must_not_create (x int)`,
	} {
		_, err := pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want permission denied (42501)", stmt, err)
		}
	}
}
