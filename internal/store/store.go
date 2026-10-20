package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Measurement struct {
	Timestamp time.Time
	EntityID  string
	Value     float64
	Unit      string
}

type pool interface {
	Begin(context.Context) (pgx.Tx, error)
	CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Ping(context.Context) error
	Close()
}

type Store struct {
	pool pool
}

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Healthy(ctx context.Context) bool {
	err := s.pool.Ping(ctx)
	return err == nil
}

func (s *Store) EnsureSchema(ctx context.Context, schemaSQL string) error {
	for _, stmt := range splitSQLStatements(schemaSQL) {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema statement %q: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) InsertMeasurements(ctx context.Context, measurements []Measurement) (int64, error) {
	if len(measurements) == 0 {
		return 0, nil
	}

	count, err := s.pool.CopyFrom(
		ctx,
		pgx.Identifier{"ha_numeric"},
		[]string{"ts", "entity_id", "value", "unit"},
		measurementRows(measurements),
	)
	if err != nil {
		return 0, fmt.Errorf("copy rows into ha_numeric: %w", err)
	}

	return count, nil
}

// ReplaceMeasurements atomically replaces [start, end) for entities represented
// in measurements. Entities without replacement data are left untouched.
func (s *Store) ReplaceMeasurements(ctx context.Context, start, end time.Time, measurements []Measurement) (int64, error) {
	if len(measurements) == 0 {
		return 0, nil
	}
	entityIDs := make([]string, 0)
	seen := make(map[string]bool)
	for _, m := range measurements {
		if m.Timestamp.Before(start) || !m.Timestamp.Before(end) {
			return 0, fmt.Errorf("replacement measurement outside [start, end)")
		}
		if !seen[m.EntityID] {
			seen[m.EntityID] = true
			entityIDs = append(entityIDs, m.EntityID)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin replacement: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx,
		"DELETE FROM ha_numeric WHERE ts >= $1 AND ts < $2 AND entity_id = ANY($3::text[])",
		start, end, entityIDs); err != nil {
		return 0, fmt.Errorf("delete replacement range: %w", err)
	}
	count, err := tx.CopyFrom(ctx, pgx.Identifier{"ha_numeric"},
		[]string{"ts", "entity_id", "value", "unit"}, measurementRows(measurements))
	if err != nil {
		return 0, fmt.Errorf("copy replacement rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit replacement: %w", err)
	}
	return count, nil
}

func measurementRows(measurements []Measurement) pgx.CopyFromSource {
	return pgx.CopyFromSlice(len(measurements), func(i int) ([]any, error) {
		m := measurements[i]
		var unit any
		if strings.TrimSpace(m.Unit) != "" {
			unit = m.Unit
		}
		return []any{m.Timestamp, m.EntityID, m.Value, unit}, nil
	})
}

// RefreshRollups recomputes the time-weighted hourly and daily rollups for
// every bucket touched by [start, end). The scheduled rollup jobs only cover
// the most recent buckets.
func (s *Store) RefreshRollups(ctx context.Context, start, end time.Time) error {
	for _, rollup := range []struct {
		table  string
		width  string
		bucket time.Duration
	}{{"ha_numeric_1h", "1 hour", time.Hour}, {"ha_numeric_1d", "1 day", 24 * time.Hour}} {
		from, to := refreshWindow(start, end, rollup.bucket)
		if _, err := s.pool.Exec(ctx,
			"SELECT ha_numeric_refresh_rollup($1::regclass, $2::interval, $3, $4)",
			rollup.table, rollup.width, from, to); err != nil {
			return fmt.Errorf("refresh %s: %w", rollup.table, err)
		}
	}
	return nil
}

func refreshWindow(start, end time.Time, bucket time.Duration) (time.Time, time.Time) {
	from := start.UTC().Truncate(bucket)
	to := end.UTC().Truncate(bucket)
	if to.Before(end) {
		to = to.Add(bucket)
	}
	return from, to
}

// splitSQLStatements splits on semicolons outside quoted strings, comments, and
// dollar-quoted bodies. Statements run one at a time because several
// TimescaleDB calls refuse to run inside the implicit transaction of a
// multi-statement query.
func splitSQLStatements(sqlText string) []string {
	var stmts []string
	start := 0
	for i := 0; i < len(sqlText); i++ {
		switch {
		case sqlText[i] == '\'':
			i = skipPast(sqlText, i+1, "'")
		case strings.HasPrefix(sqlText[i:], "--"):
			i = skipPast(sqlText, i+2, "\n")
		case strings.HasPrefix(sqlText[i:], "/*"):
			i = skipPast(sqlText, i+2, "*/")
		case sqlText[i] == '$':
			if tag := dollarQuoteTag.FindString(sqlText[i:]); tag != "" {
				i = skipPast(sqlText, i+len(tag), tag)
			}
		case sqlText[i] == ';':
			stmts = appendStatement(stmts, sqlText[start:i])
			start = i + 1
		}
	}
	return appendStatement(stmts, sqlText[start:])
}

var dollarQuoteTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

// skipPast returns the index of the last byte of the first terminator at or
// after from, or the end of text when the terminator is missing. A doubled
// single quote inside a string is found as two adjacent terminators.
func skipPast(text string, from int, terminator string) int {
	if j := strings.Index(text[from:], terminator); j >= 0 {
		return from + j + len(terminator) - 1
	}
	return len(text) - 1
}

func appendStatement(stmts []string, stmt string) []string {
	if stmt = strings.TrimSpace(stmt); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}
