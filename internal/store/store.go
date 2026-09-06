package store

import (
	"context"
	"fmt"
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

// RefreshContinuousAggregates re-materializes the hourly and daily rollups for
// every bucket touched by [start, end). Automatic refresh policies only cover
// the recent window (start_offset of 7 days).
// refresh_continuous_aggregate cannot run inside a transaction, so each call is
// issued on its own via the pool.
func (s *Store) RefreshContinuousAggregates(ctx context.Context, start, end time.Time) error {
	for _, aggregate := range []struct {
		view   string
		bucket time.Duration
	}{{"ha_numeric_1h", time.Hour}, {"ha_numeric_1d", 24 * time.Hour}} {
		from, to := refreshWindow(start, end, aggregate.bucket)
		// Simple protocol keeps CALL outside an explicit transaction, while pgx
		// handles quoting parameters.
		if _, err := s.pool.Exec(ctx,
			"CALL refresh_continuous_aggregate($1::regclass, $2::timestamptz, $3::timestamptz)",
			pgx.QueryExecModeSimpleProtocol, aggregate.view, from, to); err != nil {
			return fmt.Errorf("refresh %s: %w", aggregate.view, err)
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
