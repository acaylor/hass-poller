package store

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	hasspoller "hass-poller"
)

// legacySchemaSQL is the pre-0.3.0 continuous aggregate definition the schema
// migrates away from.
const legacySchemaSQL = `
CREATE MATERIALIZED VIEW ha_numeric_1h WITH (timescaledb.continuous) AS
SELECT time_bucket('1 hour', ts) AS bucket, entity_id,
       avg(value) AS avg, min(value) AS min, max(value) AS max, count(*) AS n
FROM ha_numeric GROUP BY bucket, entity_id WITH NO DATA;
CREATE MATERIALIZED VIEW ha_numeric_1d WITH (timescaledb.continuous) AS
SELECT time_bucket('1 day', ts) AS bucket, entity_id,
       avg(value) AS avg, min(value) AS min, max(value) AS max, count(*) AS n
FROM ha_numeric GROUP BY bucket, entity_id WITH NO DATA`

type rollupRow struct {
	Unit          *string
	Avg, Min, Max float64
	N             int64
	Seconds       *float64
}

// integrationStore returns a store confined to a fresh schema of the
// TimescaleDB database named by HASS_POLLER_TEST_DSN.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("HASS_POLLER_TEST_DSN")
	if dsn == "" {
		t.Skip("HASS_POLLER_TEST_DSN not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("hass_poller_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(ctx, "SELECT delete_job(job_id) FROM timescaledb_information.jobs WHERE proc_schema = $1 OR hypertable_schema = $1", schema)
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(ctx)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	s, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func mustExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func insertRaw(t *testing.T, s *Store, entityID, unit string, points ...any) {
	t.Helper()
	var measurements []Measurement
	for i := 0; i < len(points); i += 2 {
		measurements = append(measurements, Measurement{
			Timestamp: points[i].(time.Time), EntityID: entityID,
			Value: float64(points[i+1].(int)), Unit: unit,
		})
	}
	if _, err := s.InsertMeasurements(context.Background(), measurements); err != nil {
		t.Fatal(err)
	}
}

func rollups(t *testing.T, s *Store, table, entityID string) map[time.Time]rollupRow {
	t.Helper()
	rows, err := s.pool.(interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}).Query(context.Background(),
		"SELECT bucket, unit, avg, min, max, n, seconds FROM "+table+" WHERE entity_id = $1", entityID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[time.Time]rollupRow{}
	for rows.Next() {
		var bucket time.Time
		var r rollupRow
		if err := rows.Scan(&bucket, &r.Unit, &r.Avg, &r.Min, &r.Max, &r.N, &r.Seconds); err != nil {
			t.Fatal(err)
		}
		out[bucket.UTC()] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkRollup(t *testing.T, got map[time.Time]rollupRow, bucket time.Time, unit string, avg, min, max float64, n int64, seconds float64) {
	t.Helper()
	r, ok := got[bucket]
	if !ok {
		t.Fatalf("no rollup for %s in %+v", bucket, got)
	}
	gotUnit := ""
	if r.Unit != nil {
		gotUnit = *r.Unit
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if gotUnit != unit || !near(r.Avg, avg) || !near(r.Min, min) || !near(r.Max, max) ||
		r.N != n || r.Seconds == nil || !near(*r.Seconds, seconds) {
		secs := "NULL"
		if r.Seconds != nil {
			secs = fmt.Sprint(*r.Seconds)
		}
		t.Errorf("%s: got unit=%q avg=%v min=%v max=%v n=%d seconds=%s, want %q %v %v %v %d %v",
			bucket, gotUnit, r.Avg, r.Min, r.Max, r.N, secs, unit, avg, min, max, n, seconds)
	}
}

func TestIntegration_TimeWeightedRollups(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatalf("schema is not idempotent: %v", err)
	}

	day := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	// 0 for 59 minutes, then 60 for one minute: time average 1, not 30.
	insertRaw(t, s, "sensor.uneven", "W", at(10, 0), 0, at(10, 59), 60, at(11, 0), 0)
	// A constant value kept alive by heartbeats across the midnight boundary.
	var heartbeats []any
	for m := 23 * 60; m <= 25*60; m += 15 {
		heartbeats = append(heartbeats, at(0, m), 5)
	}
	insertRaw(t, s, "sensor.constant", "°C", heartbeats...)
	// A value with no later row expires after the one-hour hold.
	insertRaw(t, s, "sensor.stopped", "", at(10, 30), 7)
	// A convertible unit change: 10 °C (50 °F) for 30 minutes, then 68 °F.
	insertRaw(t, s, "sensor.temp", "°C", at(10, 0), 10)
	insertRaw(t, s, "sensor.temp", "°F", at(10, 30), 68, at(11, 0), 68)
	// An incompatible unit change keeps only the final unit's values.
	insertRaw(t, s, "sensor.air", "%", at(10, 0), 50)
	insertRaw(t, s, "sensor.air", "ppm", at(10, 30), 3, at(11, 0), 3)

	if err := s.RefreshRollups(ctx, day, day.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	hourly := rollups(t, s, "ha_numeric_1h", "sensor.uneven")
	checkRollup(t, hourly, at(10, 0), "W", 1, 0, 60, 2, 3600)

	hourly = rollups(t, s, "ha_numeric_1h", "sensor.constant")
	checkRollup(t, hourly, at(23, 0), "°C", 5, 5, 5, 4, 3600)
	checkRollup(t, hourly, at(24, 0), "°C", 5, 5, 5, 4, 3600)
	checkRollup(t, hourly, at(25, 0), "°C", 5, 5, 5, 1, 3600)
	daily := rollups(t, s, "ha_numeric_1d", "sensor.constant")
	checkRollup(t, daily, day, "°C", 5, 5, 5, 4, 3600)
	checkRollup(t, daily, day.Add(24*time.Hour), "°C", 5, 5, 5, 5, 2*3600)

	hourly = rollups(t, s, "ha_numeric_1h", "sensor.stopped")
	checkRollup(t, hourly, at(10, 0), "", 7, 7, 7, 1, 1800)
	checkRollup(t, hourly, at(11, 0), "", 7, 7, 7, 0, 1800)
	if len(hourly) != 2 {
		t.Errorf("value outlived its hold: %+v", hourly)
	}

	hourly = rollups(t, s, "ha_numeric_1h", "sensor.temp")
	checkRollup(t, hourly, at(10, 0), "°F", 59, 50, 68, 2, 3600)

	hourly = rollups(t, s, "ha_numeric_1h", "sensor.air")
	checkRollup(t, hourly, at(10, 0), "ppm", 3, 3, 3, 1, 1800)

	// A partial refresh covers whole buckets and carries the value in effect
	// before its range.
	mustExec(t, s, "DELETE FROM ha_numeric_1h")
	if err := s.RefreshRollups(ctx, at(11, 5), at(11, 10)); err != nil {
		t.Fatal(err)
	}
	hourly = rollups(t, s, "ha_numeric_1h", "sensor.stopped")
	checkRollup(t, hourly, at(11, 0), "", 7, 7, 7, 0, 1800)
	if len(hourly) != 1 {
		t.Errorf("refresh wrote buckets outside its range: %+v", hourly)
	}
	if got := rollups(t, s, "ha_numeric_1h", "sensor.constant"); len(got) != 0 {
		t.Errorf("refresh wrote buckets outside its range: %+v", got)
	}

	var jobs, retention int
	if err := s.pool.(interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}).QueryRow(ctx, `SELECT
		  count(*) FILTER (WHERE proc_name = 'ha_numeric_rollup_job'),
		  count(*) FILTER (WHERE proc_name = 'policy_retention' AND hypertable_name = 'ha_numeric_1h')
		FROM timescaledb_information.jobs WHERE proc_schema = current_schema() OR hypertable_schema = current_schema()`,
	).Scan(&jobs, &retention); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 || retention != 1 {
		t.Errorf("got %d rollup jobs and %d hourly retention policies, want 2 and 1", jobs, retention)
	}
	// The scheduled jobs must run with their registered config.
	for _, table := range []string{"ha_numeric_1h", "ha_numeric_1d"} {
		var jobID int
		if err := s.pool.(interface {
			QueryRow(context.Context, string, ...any) pgx.Row
		}).QueryRow(ctx, `SELECT job_id FROM timescaledb_information.jobs
			WHERE proc_schema = current_schema() AND config->>'rollup' = $1`, table).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, "CALL run_job($1)", pgx.QueryExecModeSimpleProtocol, jobID); err != nil {
			t.Fatalf("run %s job: %v", table, err)
		}
	}
}

func TestIntegration_MigratesContinuousAggregates(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

	// Build the pre-0.3.0 schema with materialized buckets, then drop the old
	// raw rows as the retention policy would.
	for _, stmt := range splitSQLStatements(hasspoller.SchemaSQL) {
		if strings.HasPrefix(stmt, "SELECT add_retention_policy('ha_numeric'") {
			break
		}
		mustExec(t, s, stmt)
	}
	for _, stmt := range splitSQLStatements(legacySchemaSQL) {
		mustExec(t, s, stmt)
	}
	insertRaw(t, s, "sensor.power", "W", old, 10, old.Add(30*time.Minute), 20)
	insertRaw(t, s, "sensor.power", "W", recent, 0, recent.Add(59*time.Minute), 60, recent.Add(time.Hour), 0)
	for _, view := range []string{"ha_numeric_1h", "ha_numeric_1d"} {
		if _, err := s.pool.Exec(ctx, "CALL refresh_continuous_aggregate($1::regclass, NULL, $2::timestamptz)",
			pgx.QueryExecModeSimpleProtocol, view, recent.Add(48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s, "DELETE FROM ha_numeric WHERE ts < $1", recent)

	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}

	hourly := rollups(t, s, "ha_numeric_1h", "sensor.power")
	if r := hourly[old]; r.Seconds != nil || r.Unit != nil || r.Avg != 15 || r.N != 2 {
		t.Errorf("legacy bucket not preserved: %+v", r)
	}
	checkRollup(t, hourly, recent, "W", 1, 0, 60, 2, 3600)
	daily := rollups(t, s, "ha_numeric_1d", "sensor.power")
	if r := daily[old]; r.Seconds != nil || r.Avg != 15 {
		t.Errorf("legacy daily bucket not preserved: %+v", r)
	}
	checkRollup(t, daily, recent, "W", 0.5, 0, 60, 3, 2*3600)

	var legacy int
	if err := s.pool.(interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}).QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname LIKE 'ha_numeric_%_legacy' AND relnamespace = current_schema()::regnamespace").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Errorf("%d legacy relations left behind", legacy)
	}
}
