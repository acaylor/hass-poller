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
