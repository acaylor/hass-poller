package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Measurement struct {
	Timestamp time.Time
	EntityID  string
	Value     float64
	Unit      string
}

type Store struct {
	pool *pgxpool.Pool
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

	rows := make([][]any, 0, len(measurements))
	for _, m := range measurements {
		var unit any
		if strings.TrimSpace(m.Unit) != "" {
			unit = m.Unit
		}
		rows = append(rows, []any{m.Timestamp, m.EntityID, m.Value, unit})
	}

	count, err := s.pool.CopyFrom(
		ctx,
		pgx.Identifier{"ha_numeric"},
		[]string{"ts", "entity_id", "value", "unit"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return 0, fmt.Errorf("copy rows into ha_numeric: %w", err)
	}

	return count, nil
}

// DeleteRange removes all rows whose ts falls in [start, end). It returns the
// number of rows deleted. Used by backfill --replace to clear a range before
// re-inserting. Note: this fails on compressed chunks (older than the
// compression policy window) unless they are decompressed first.
func (s *Store) DeleteRange(ctx context.Context, start, end time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM ha_numeric WHERE ts >= $1 AND ts < $2", start, end)
	if err != nil {
		return 0, fmt.Errorf("delete range: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RefreshContinuousAggregates re-materializes the hourly and daily rollups for
// [start, end]. Required after backfilling old data, because the automatic
// refresh policies only cover the recent window (start_offset of 7 days).
// refresh_continuous_aggregate cannot run inside a transaction, so each call is
// issued on its own via the pool.
func (s *Store) RefreshContinuousAggregates(ctx context.Context, start, end time.Time) error {
	// Procedure names and timestamps are internal/trusted (not user input), and
	// refresh_continuous_aggregate must run via simple query protocol because it
	// commits internally and cannot accept bound parameters.
	for _, view := range []string{"ha_numeric_1h", "ha_numeric_1d"} {
		sql := fmt.Sprintf(
			"CALL refresh_continuous_aggregate('%s', '%s'::timestamptz, '%s'::timestamptz)",
			view,
			start.UTC().Format(time.RFC3339Nano),
			end.UTC().Format(time.RFC3339Nano),
		)
		if _, err := s.pool.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("refresh %s: %w", view, err)
		}
	}
	return nil
}

func splitSQLStatements(sqlText string) []string {
	parts := strings.Split(sqlText, ";")
	stmts := make([]string, 0, len(parts))
	for _, part := range parts {
		stmt := strings.TrimSpace(part)
		if stmt == "" {
			continue
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}
