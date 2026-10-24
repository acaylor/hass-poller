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

// testDay returns a UTC midnight recent enough that the raw retention policy,
// which can run as soon as startup adds it, keeps the test's rows, and old
// enough that the buckets the tests check have completed.
func testDay() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
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

// rollupKey identifies a rollup row of one entity. Unit is empty for NULL.
type rollupKey struct {
	Bucket time.Time
	Unit   string
}

func rollups(t *testing.T, s *Store, table, entityID string) map[rollupKey]rollupRow {
	t.Helper()
	rows, err := s.pool.(interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}).Query(context.Background(),
		"SELECT bucket, unit, avg, min, max, n, seconds FROM "+table+" WHERE entity_id = $1", entityID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[rollupKey]rollupRow{}
	for rows.Next() {
		var bucket time.Time
		var r rollupRow
		if err := rows.Scan(&bucket, &r.Unit, &r.Avg, &r.Min, &r.Max, &r.N, &r.Seconds); err != nil {
			t.Fatal(err)
		}
		key := rollupKey{Bucket: bucket.UTC()}
		if r.Unit != nil {
			key.Unit = *r.Unit
		}
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkRollup(t *testing.T, got map[rollupKey]rollupRow, bucket time.Time, unit string, avg, min, max float64, n int64, seconds float64) {
	t.Helper()
	r, ok := got[rollupKey{bucket, unit}]
	if !ok {
		t.Fatalf("no %q rollup for %s in %+v", unit, bucket, got)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(r.Avg, avg) || !near(r.Min, min) || !near(r.Max, max) ||
		r.N != n || r.Seconds == nil || !near(*r.Seconds, seconds) {
		secs := "NULL"
		if r.Seconds != nil {
			secs = fmt.Sprint(*r.Seconds)
		}
		t.Errorf("%s %q: got avg=%v min=%v max=%v n=%d seconds=%s, want %v %v %v %d %v",
			bucket, unit, r.Avg, r.Min, r.Max, r.N, secs, avg, min, max, n, seconds)
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

	day := testDay()
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
	// A unit change splits the bucket into one series per unit. The first
	// unit's value ends at the first row in the new unit.
	insertRaw(t, s, "sensor.temp", "°C", at(10, 0), 10)
	insertRaw(t, s, "sensor.temp", "°F", at(10, 30), 68, at(10, 45), 70, at(11, 0), 70)
	// A unitless value is its own series too.
	insertRaw(t, s, "sensor.air", "", at(10, 0), 50)
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
	checkRollup(t, hourly, at(10, 0), "°C", 10, 10, 10, 1, 1800)
	checkRollup(t, hourly, at(10, 0), "°F", 69, 68, 70, 2, 1800)
	checkRollup(t, hourly, at(11, 0), "°F", 70, 70, 70, 1, 3600)
	if len(hourly) != 3 {
		t.Errorf("unexpected unit series: %+v", hourly)
	}

	hourly = rollups(t, s, "ha_numeric_1h", "sensor.air")
	checkRollup(t, hourly, at(10, 0), "", 50, 50, 50, 1, 1800)
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

func TestIntegration_RefreshCoversHeldValues(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	day := testDay()
	midnight := day.Add(24 * time.Hour)

	// The poller resumes at 00:10 after a gap ending at midnight.
	insertRaw(t, s, "sensor.gap", "W", midnight.Add(10*time.Minute), 4)
	if err := s.RefreshRollups(ctx, day, midnight.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	checkRollup(t, rollups(t, s, "ha_numeric_1h", "sensor.gap"), midnight, "W", 4, 4, 4, 1, 3000)

	// Backfilling the gap must refresh the buckets after it that its last
	// heartbeat holds into.
	insertRaw(t, s, "sensor.gap", "W", day.Add(23*time.Hour+45*time.Minute), 4)
	if err := s.RefreshRollups(ctx, day.Add(23*time.Hour), midnight); err != nil {
		t.Fatal(err)
	}
	checkRollup(t, rollups(t, s, "ha_numeric_1h", "sensor.gap"), midnight, "W", 4, 4, 4, 1, 3600)
	daily := rollups(t, s, "ha_numeric_1d", "sensor.gap")
	checkRollup(t, daily, day, "W", 4, 4, 4, 1, 900)
	checkRollup(t, daily, midnight, "W", 4, 4, 4, 1, 4200)

	// A hold raised in the job config applies to the refresh and its range.
	mustExec(t, s, `SELECT alter_job(job_id, config => config || '{"hold": "2 hours"}')
		FROM timescaledb_information.jobs
		WHERE proc_schema = current_schema() AND proc_name = 'ha_numeric_rollup_job'`)
	held := day.Add(22*time.Hour + 30*time.Minute)
	insertRaw(t, s, "sensor.held", "", held, 1)
	if err := s.RefreshRollups(ctx, held, held.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	checkRollup(t, rollups(t, s, "ha_numeric_1h", "sensor.held"), midnight, "", 1, 1, 1, 0, 1800)

	// Incomplete buckets are left to the scheduled jobs.
	now := time.Now().UTC()
	insertRaw(t, s, "sensor.recent", "", now.Add(-2*time.Hour), 1, now.Add(-time.Minute), 1)
	if err := s.RefreshRollups(ctx, now.Add(-2*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	for table, width := range map[string]time.Duration{"ha_numeric_1h": time.Hour, "ha_numeric_1d": 24 * time.Hour} {
		for key := range rollups(t, s, table, "sensor.recent") {
			if !key.Bucket.Before(now.Truncate(width)) {
				t.Errorf("%s wrote incomplete bucket %s", table, key.Bucket)
			}
		}
	}
}

func TestIntegration_MigratesContinuousAggregates(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	recent := testDay()
	old := recent.AddDate(0, 0, -19)

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
	// An older backfill of another sensor moves the earliest raw row back.
	insertRaw(t, s, "sensor.other", "W", old.Add(10*time.Minute), 1)

	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}

	checkMigrated := func() {
		t.Helper()
		hourly := rollups(t, s, "ha_numeric_1h", "sensor.power")
		if r := hourly[rollupKey{Bucket: old}]; r.Seconds != nil || r.Unit != nil || r.Avg != 15 || r.N != 2 {
			t.Errorf("legacy bucket not preserved: %+v", r)
		}
		checkRollup(t, hourly, recent, "W", 1, 0, 60, 2, 3600)
		if r, ok := hourly[rollupKey{Bucket: recent}]; ok {
			t.Errorf("recomputed bucket kept its legacy row: %+v", r)
		}
		daily := rollups(t, s, "ha_numeric_1d", "sensor.power")
		if r := daily[rollupKey{Bucket: old}]; r.Seconds != nil || r.Avg != 15 {
			t.Errorf("legacy daily bucket not preserved: %+v", r)
		}
		checkRollup(t, daily, recent, "W", 0.5, 0, 60, 3, 2*3600)
		checkRollup(t, rollups(t, s, "ha_numeric_1h", "sensor.other"), old, "W", 1, 1, 1, 1, 3000)
	}
	checkMigrated()
	// Refreshing a range whose raw data has expired keeps the rollups of the
	// entities without raw rows there, and the buckets of an entity whose raw
	// rows cover only part of the range.
	for _, end := range []time.Time{old.Add(30 * time.Minute), recent.Add(time.Hour)} {
		if err := s.RefreshRollups(ctx, old, end); err != nil {
			t.Fatal(err)
		}
		checkMigrated()
	}

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

func TestIntegration_BlockedWriteHonorsDeadline(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "LOCK TABLE ha_numeric IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = s.InsertMeasurements(writeCtx, []Measurement{{Timestamp: started, EntityID: "sensor.temp", Value: 1}})
	if err == nil {
		t.Fatal("write succeeded through an exclusive lock")
	}
	if waited := time.Since(started); waited < 500*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("blocked write returned after %s", waited)
	}
	if !s.Healthy(ctx) {
		t.Fatal("pool unusable after a cancelled write")
	}
}

func TestIntegration_StagingReplacesAtomically(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	start := testDay()
	end := start.Add(2 * time.Hour)
	insertRaw(t, s, "sensor.replaced", "W", start, 1, start.Add(time.Hour), 2, end, 3)
	insertRaw(t, s, "sensor.untouched", "W", start.Add(time.Hour), 4)

	count := func() map[string]int {
		rows, err := s.pool.(interface {
			Query(context.Context, string, ...any) (pgx.Rows, error)
		}).Query(ctx, "SELECT entity_id, count(*) FROM ha_numeric GROUP BY entity_id")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			out[id] = n
		}
		return out
	}

	// A rolled-back staging run writes nothing.
	st, err := s.BeginStaging(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(ctx, []Measurement{{Timestamp: start, EntityID: "sensor.replaced", Value: 9}}); err != nil {
		t.Fatal(err)
	}
	st.Rollback()
	if got := count(); got["sensor.replaced"] != 3 || got["sensor.untouched"] != 1 {
		t.Fatalf("rollback changed rows: %v", got)
	}

	// Chunked replacement swaps only the staged entity's rows inside the range;
	// the temporary table can be created again in the same session.
	for range 2 {
		st, err = s.BeginStaging(ctx, start, end)
		if err != nil {
			t.Fatal(err)
		}
		for _, ts := range []time.Time{start.Add(time.Minute), start.Add(90 * time.Minute)} {
			if err := st.Add(ctx, []Measurement{{Timestamp: ts, EntityID: "sensor.replaced", Value: 5, Unit: "W"}}); err != nil {
				t.Fatal(err)
			}
		}
		n, err := st.Commit(ctx, true)
		if err != nil || n != 2 {
			t.Fatalf("commit: %d %v", n, err)
		}
	}
	// Two replacement rows plus the untouched row at end.
	if got := count(); got["sensor.replaced"] != 3 || got["sensor.untouched"] != 1 {
		t.Fatalf("replacement scope wrong: %v", got)
	}
}
