package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty input",
			in:   "",
			want: []string{},
		},
		{
			name: "whitespace only",
			in:   "   \n\t  ",
			want: []string{},
		},
		{
			name: "single statement without trailing semicolon",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "trailing semicolon does not yield an empty statement",
			in:   "SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "multiple statements are trimmed and split",
			in:   "CREATE TABLE t (id int);\n\nSELECT 1;\n",
			want: []string{"CREATE TABLE t (id int)", "SELECT 1"},
		},
		{
			name: "blank statements between semicolons are dropped",
			in:   "SELECT 1;;\n  ;SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "dollar-quoted bodies are not split",
			in:   "DO $$ BEGIN PERFORM 1; END $$;\nCREATE FUNCTION f() RETURNS int LANGUAGE sql AS $fn$ SELECT 1; $fn$;",
			want: []string{"DO $$ BEGIN PERFORM 1; END $$", "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $fn$ SELECT 1; $fn$"},
		},
		{
			name: "strings and comments are not split",
			in:   "SELECT 'a;''b' -- c;d\n/* e;f */;SELECT $1",
			want: []string{"SELECT 'a;''b' -- c;d\n/* e;f */", "SELECT $1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQLStatements(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("statement %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Unimplemented methods panic through the embedded interfaces so unexpected
// database operations fail the test rather than silently succeeding.
type transactionPool struct {
	pool
	tx *stagingTx
}

func (p *transactionPool) Begin(context.Context) (pgx.Tx, error) { return p.tx, nil }

type stagingTx struct {
	pgx.Tx
	execs      []string
	deleteArgs []any
	rows       [][]any
	copyErr    error
	commitErr  error
	committed  bool
	rolledBack bool
}

func (tx *stagingTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.execs = append(tx.execs, strings.Fields(sql)[0])
	switch {
	case strings.HasPrefix(sql, "DELETE"):
		if !strings.Contains(sql, "entity_id IN (SELECT DISTINCT entity_id FROM ha_numeric_staging)") {
			return pgconn.CommandTag{}, errors.New("replacement delete must restrict entity IDs")
		}
		tx.deleteArgs = args
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.HasPrefix(sql, "INSERT"):
		return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", len(tx.rows))), nil
	}
	return pgconn.NewCommandTag("CREATE TABLE"), nil
}
func (tx *stagingTx) CopyFrom(_ context.Context, table pgx.Identifier, _ []string, rows pgx.CopyFromSource) (int64, error) {
	if tx.copyErr != nil {
		return 0, tx.copyErr
	}
	if table[0] != "ha_numeric_staging" {
		return 0, errors.New("rows must be staged")
	}
	for rows.Next() {
		row, err := rows.Values()
		if err != nil {
			return 0, err
		}
		tx.rows = append(tx.rows, row)
	}
	return int64(len(tx.rows)), rows.Err()
}
func (tx *stagingTx) Commit(context.Context) error {
	if tx.commitErr != nil {
		return tx.commitErr
	}
	tx.committed = true
	return nil
}
func (tx *stagingTx) Rollback(context.Context) error {
	if !tx.committed {
		tx.rolledBack = true
	}
	return nil
}

func TestStaging_AtomicAndScoped(t *testing.T) {
	start := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	measurements := []Measurement{
		{Timestamp: start, EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: start.Add(time.Minute), EntityID: "sensor.temp", Value: 21, Unit: ""},
	}
	for _, failure := range []string{"", "copy", "commit"} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure=%s replace=%v", failure, replace), func(t *testing.T) {
				tx := &stagingTx{}
				failureErr := errors.New("database failure")
				if failure == "copy" {
					tx.copyErr = failureErr
				}
				if failure == "commit" {
					tx.commitErr = failureErr
				}
				st, err := (&Store{pool: &transactionPool{tx: tx}}).BeginStaging(context.Background(), start, end)
				if err != nil {
					t.Fatal(err)
				}
				defer st.Rollback()
				// Rows arrive in chunks.
				err = st.Add(context.Background(), measurements[:1])
				if err == nil {
					err = st.Add(context.Background(), measurements[1:])
				}
				count := int64(0)
				if err == nil {
					count, err = st.Commit(context.Background(), replace)
				}
				if failure != "" {
					st.Rollback() // Callers defer this; Commit also rolls back on failure.
					if !errors.Is(err, failureErr) || count != 0 || tx.committed || !tx.rolledBack {
						t.Fatalf("failed staging: count=%d err=%v committed=%v rolledBack=%v", count, err, tx.committed, tx.rolledBack)
					}
					return
				}
				if err != nil || count != 2 || !tx.committed {
					t.Fatalf("count=%d err=%v committed=%v", count, err, tx.committed)
				}
				if tx.rows[0][3] != "°C" || tx.rows[1][3] != nil {
					t.Fatalf("incorrect unit encoding: %+v", tx.rows)
				}
				wantExecs := []string{"CREATE", "INSERT"}
				if replace {
					wantExecs = []string{"CREATE", "DELETE", "INSERT"}
					if !reflect.DeepEqual(tx.deleteArgs, []any{start, end}) {
						t.Fatalf("unexpected replacement range: %+v", tx.deleteArgs)
					}
				}
				if !reflect.DeepEqual(tx.execs, wantExecs) {
					t.Fatalf("got statements %v, want %v", tx.execs, wantExecs)
				}
			})
		}
	}

	// Out-of-range rows are rejected before reaching the database.
	st, err := (&Store{pool: &transactionPool{tx: &stagingTx{}}}).BeginStaging(context.Background(), start, end)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Rollback()
	if err := st.Add(context.Background(), []Measurement{{Timestamp: end, EntityID: "sensor.temp"}}); err == nil {
		t.Fatal("accepted out-of-range measurement")
	}
}

type execPool struct {
	pool
	execs [][]any
}

func (p *execPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.execs = append(p.execs, append([]any{sql}, args...))
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func TestRefreshRollups(t *testing.T) {
	p := &execPool{}
	// The database aligns the range to buckets, so it is passed through in UTC.
	start := time.Date(2026, 6, 20, 4, 30, 0, 0, time.FixedZone("offset", -6*60*60))
	end := start.Add(time.Hour)
	if err := (&Store{pool: p}).RefreshRollups(context.Background(), start, end); err != nil {
		t.Fatal(err)
	}
	const sql = "SELECT ha_numeric_refresh_rollup($1::regclass, $2::interval, $3, $4)"
	want := [][]any{
		{sql, "ha_numeric_1h", "1 hour", start.UTC(), end.UTC()},
		{sql, "ha_numeric_1d", "1 day", start.UTC(), end.UTC()},
	}
	if !reflect.DeepEqual(p.execs, want) {
		t.Fatalf("got %+v\nwant %+v", p.execs, want)
	}
}
