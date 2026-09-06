package store

import (
	"context"
	"errors"
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
	tx *replacementTx
}

func (p *transactionPool) Begin(context.Context) (pgx.Tx, error) { return p.tx, nil }

type replacementTx struct {
	pgx.Tx
	deleteArgs []any
	rows       [][]any
	copyErr    error
	commitErr  error
	committed  bool
	rolledBack bool
}

func (tx *replacementTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "entity_id = ANY($3::text[])") {
		return pgconn.CommandTag{}, errors.New("replacement delete must restrict entity IDs")
	}
	tx.deleteArgs = args
	return pgconn.NewCommandTag("DELETE 1"), nil
}
func (tx *replacementTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, rows pgx.CopyFromSource) (int64, error) {
	if tx.copyErr != nil {
		return 0, tx.copyErr
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
func (tx *replacementTx) Commit(context.Context) error {
	if tx.commitErr != nil {
		return tx.commitErr
	}
	tx.committed = true
	return nil
}
func (tx *replacementTx) Rollback(context.Context) error {
	if !tx.committed {
		tx.rolledBack = true
	}
	return nil
}

func TestReplaceMeasurements_AtomicAndScoped(t *testing.T) {
	start := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	measurements := []Measurement{
		{Timestamp: start, EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: start.Add(time.Minute), EntityID: "sensor.temp", Value: 21, Unit: ""},
	}
	for _, failure := range []string{"", "copy", "commit"} {
		t.Run(failure, func(t *testing.T) {
			tx := &replacementTx{}
			failureErr := errors.New("database failure")
			if failure == "copy" {
				tx.copyErr = failureErr
			}
			if failure == "commit" {
				tx.commitErr = failureErr
			}
			s := &Store{pool: &transactionPool{tx: tx}}
			count, err := s.ReplaceMeasurements(context.Background(), start, end, measurements)
			if failure != "" {
				if !errors.Is(err, failureErr) || count != 0 || tx.committed || !tx.rolledBack {
					t.Fatalf("failed replacement: count=%d err=%v committed=%v rolledBack=%v", count, err, tx.committed, tx.rolledBack)
				}
			} else {
				if err != nil || count != 2 || !tx.committed {
					t.Fatalf("count=%d err=%v committed=%v", count, err, tx.committed)
				}
				if tx.rows[0][3] != "°C" || tx.rows[1][3] != nil {
					t.Fatalf("incorrect unit encoding: %+v", tx.rows)
				}
			}
			if !reflect.DeepEqual(tx.deleteArgs, []any{start, end, []string{"sensor.temp"}}) {
				t.Fatalf("unexpected replacement scope: %+v", tx.deleteArgs)
			}
		})
	}
	// Empty and out-of-range replacements must not even open a transaction.
	s := &Store{}
	if n, err := s.ReplaceMeasurements(context.Background(), start, end, nil); n != 0 || err != nil {
		t.Fatalf("empty replacement: %d %v", n, err)
	}
	measurements[0].Timestamp = end
	if _, err := s.ReplaceMeasurements(context.Background(), start, end, measurements); err == nil {
		t.Fatal("accepted out-of-range replacement")
	}
}

func TestRefreshWindow(t *testing.T) {
	zone := time.FixedZone("offset", -6*60*60)
	start := time.Date(2026, 6, 20, 10, 30, 0, 0, zone)
	end := start.Add(20 * time.Minute)
	for _, tt := range []struct {
		bucket   time.Duration
		from, to time.Time
	}{
		{time.Hour, time.Date(2026, 6, 20, 16, 0, 0, 0, time.UTC), time.Date(2026, 6, 20, 17, 0, 0, 0, time.UTC)},
		{24 * time.Hour, time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)},
	} {
		from, to := refreshWindow(start, end, tt.bucket)
		if !from.Equal(tt.from) || !to.Equal(tt.to) {
			t.Fatalf("bucket %s: got %s..%s, want %s..%s", tt.bucket, from, to, tt.from, tt.to)
		}
		// An aligned exclusive end must not refresh the following bucket.
		from, to = refreshWindow(tt.from, tt.to, tt.bucket)
		if !from.Equal(tt.from) || !to.Equal(tt.to) {
			t.Fatalf("aligned boundaries changed: %s..%s", from, to)
		}
	}
}
