// Package store owns all Postgres access. Other packages never write SQL.
package store

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/jackc/pgx/v5"
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

// AddCompany inserts a company, or updates its name if (ats, slug) already exists.
// Safe to run any number of times.
func (s *Store) AddCompany(ctx context.Context, name, ats, slug string) error {
	// Placeholders ($1..$3), never fmt.Sprintf: Postgres treats the values as data, not SQL.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO companies (name, ats, slug) VALUES ($1, $2, $3)
		ON CONFLICT (ats, slug) DO UPDATE SET name = EXCLUDED.name`,
		name, ats, slug)
	if err != nil {
		return fmt.Errorf("add company %s/%s: %w", ats, slug, err)
	}
	return nil
}

// GetCompany looks up one company by its board.
func (s *Store) GetCompany(ctx context.Context, ats, slug string) (Company, error) {
	var c Company
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, ats, slug, baseline_done FROM companies
		WHERE ats = $1 AND slug = $2`, ats, slug,
	).Scan(&c.ID, &c.Name, &c.ATS, &c.Slug, &c.BaselineDone)
	if err != nil {
		return Company{}, fmt.Errorf("get company %s/%s: %w", ats, slug, err)
	}
	return c, nil
}

// CrawlResult is what one crawl of a board changed.
type CrawlResult struct {
	NewJobIDs []int64 // jobs we had never seen before
	Baseline  bool    // first crawl of this company: NewJobIDs were already on the board, so don't alert
}

// RecordCrawl saves a company's current postings and reports which are new.
// Saving the jobs and marking the baseline done commit together or not at all.
func (s *Store) RecordCrawl(ctx context.Context, companyID int64, jobs []fetcher.Job) (CrawlResult, error) {
	var res CrawlResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR UPDATE locks the company row until commit, so two crawls of the
		// same board can't both read baseline_done = false.
		var baselineDone bool
		err := tx.QueryRow(ctx, `SELECT baseline_done FROM companies WHERE id = $1 FOR UPDATE`,
			companyID).Scan(&baselineDone)
		if err != nil {
			return err
		}

		ids, err := insertJobs(ctx, tx, companyID, jobs)
		if err != nil {
			return err
		}

		if !baselineDone {
			if _, err := tx.Exec(ctx, `UPDATE companies SET baseline_done = true WHERE id = $1`, companyID); err != nil {
				return err
			}
		}
		res = CrawlResult{NewJobIDs: ids, Baseline: !baselineDone}
		return nil
	})
	if err != nil {
		return CrawlResult{}, fmt.Errorf("record crawl for company %d: %w", companyID, err)
	}
	return res, nil
}

// insertJobs adds the jobs not already stored and returns their IDs.
func insertJobs(ctx context.Context, tx pgx.Tx, companyID int64, jobs []fetcher.Job) ([]int64, error) {
	// One round trip for the whole board: send each column as an array and let
	// unnest turn them back into rows. RETURNING only yields rows actually inserted.
	ids := make([]string, len(jobs))
	titles := make([]string, len(jobs))
	locations := make([]string, len(jobs))
	urls := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i], titles[i], locations[i], urls[i] = j.ID, j.Title, j.Location, j.URL
	}

	rows, err := tx.Query(ctx, `
		INSERT INTO jobs (company_id, external_id, title, location, url)
		SELECT $1, * FROM unnest($2::text[], $3::text[], $4::text[], $5::text[])
		ON CONFLICT (company_id, external_id) DO NOTHING
		RETURNING id`,
		companyID, ids, titles, locations, urls)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
