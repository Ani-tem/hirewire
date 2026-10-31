// Package store owns all Postgres access. Other packages never write SQL.
package store

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Company is a job board we crawl.
type Company struct {
	ID           int64
	Name         string
	ATS          string // "greenhouse", "lever" or "ashby"
	Slug         string
	BaselineDone bool
}

// Subscription is one user's filter. A job matches when its title contains any
// include keyword, none of the exclude keywords, and its location matches (if any are set).
type Subscription struct {
	ID              int64
	UserID          int64
	IncludeKeywords []string
	ExcludeKeywords []string
	Locations       []string
}

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and applies the schema.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }
