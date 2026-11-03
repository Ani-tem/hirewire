// Package store owns all Postgres access. Other packages never write SQL.
package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/match"
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

// StoredJob is a job as saved, with its company's name.
type StoredJob struct {
	Company  string
	Title    string
	Location string
	URL      string
}

// ListJobs returns every stored job, newest first.
func (s *Store) ListJobs(ctx context.Context) ([]StoredJob, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.name, j.title, j.location, j.url
		FROM jobs j JOIN companies c ON c.id = j.company_id
		ORDER BY j.first_seen_at DESC, j.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[StoredJob])
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobs, nil
}

// ClaimDueCompanies hands out up to limit companies that are due for a crawl.
// Claiming pushes next_poll_at forward by lease, so no other worker picks the
// same company up; if the crawler dies mid-crawl, the lease simply runs out
// and the company becomes due again.
func (s *Store) ClaimDueCompanies(ctx context.Context, limit int, lease time.Duration) ([]Company, error) {
	// SKIP LOCKED: rows another worker is claiming right now are skipped, not waited on.
	rows, err := s.pool.Query(ctx, `
		UPDATE companies SET next_poll_at = now() + make_interval(secs => $2)
		WHERE id IN (
			SELECT id FROM companies
			WHERE enabled AND next_poll_at <= now()
			ORDER BY next_poll_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, name, ats, slug, baseline_done`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim due companies: %w", err)
	}
	companies, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Company])
	if err != nil {
		return nil, fmt.Errorf("claim due companies: %w", err)
	}
	return companies, nil
}

// RecordFailure counts a failed crawl and backs off: the next poll waits
// poll_interval × 2^failures, capped at 6 hours, so a broken board isn't hammered.
func (s *Store) RecordFailure(ctx context.Context, companyID int64) error {
	// On the right of SET, consecutive_failures is still the old value.
	_, err := s.pool.Exec(ctx, `
		UPDATE companies SET
			consecutive_failures = consecutive_failures + 1,
			next_poll_at = now() + LEAST(poll_interval * power(2, consecutive_failures + 1), interval '6 hours')
		WHERE id = $1`, companyID)
	if err != nil {
		return fmt.Errorf("record failure for company %d: %w", companyID, err)
	}
	return nil
}

// AddUser registers a Telegram chat and returns the user's ID.
// A chat that comes back after leaving is made active again.
func (s *Store) AddUser(ctx context.Context, telegramChatID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (telegram_chat_id) VALUES ($1)
		ON CONFLICT (telegram_chat_id) DO UPDATE SET active = true
		RETURNING id`, telegramChatID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add user %d: %w", telegramChatID, err)
	}
	return id, nil
}

// AddSubscription saves a filter for a user and returns its ID.
func (s *Store) AddSubscription(ctx context.Context, userID int64, f match.Filter) (int64, error) {
	// A nil slice reaches Postgres as NULL, not an empty array; COALESCE keeps NOT NULL happy.
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (user_id, include_keywords, exclude_keywords, locations)
		VALUES ($1, COALESCE($2::text[], '{}'), COALESCE($3::text[], '{}'), COALESCE($4::text[], '{}'))
		RETURNING id`, userID, f.Include, f.Exclude, f.Locations).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add subscription for user %d: %w", userID, err)
	}
	return id, nil
}

// CrawlResult is what one crawl of a board changed.
type CrawlResult struct {
	NewJobIDs []int64 // jobs we had never seen before
	Baseline  bool    // first crawl of this company: NewJobIDs were already on the board, so don't alert
	Notified  int     // notifications queued for the new jobs
}

// RecordCrawl saves a company's current postings, reports which are new and
// queues a notification for every user whose subscription matches one.
// Jobs, notifications and the baseline flag commit together or not at all:
// if we crash before commit, the next crawl sees the same jobs as new and
// tries again, so an alert can be late but never lost.
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

		inserted, err := insertJobs(ctx, tx, companyID, jobs)
		if err != nil {
			return err
		}
		notified := 0
		if baselineDone && len(inserted) > 0 { // the baseline is what was already there, not news
			if notified, err = queueNotifications(ctx, tx, inserted); err != nil {
				return err
			}
		}

		// A successful crawl ends the baseline, clears any failure streak and books the next poll.
		_, err = tx.Exec(ctx, `
			UPDATE companies SET baseline_done = true, last_success_at = now(),
				consecutive_failures = 0, next_poll_at = now() + poll_interval
			WHERE id = $1`, companyID)
		if err != nil {
			return err
		}
		ids := make([]int64, len(inserted))
		for i, j := range inserted {
			ids[i] = j.ID
		}
		res = CrawlResult{NewJobIDs: ids, Baseline: !baselineDone, Notified: notified}
		return nil
	})
	if err != nil {
		return CrawlResult{}, fmt.Errorf("record crawl for company %d: %w", companyID, err)
	}
	return res, nil
}

// newJob is a just-inserted job, with what matching needs.
type newJob struct {
	ID       int64
	Title    string
	Location string
}

// insertJobs adds the jobs not already stored and returns them.
func insertJobs(ctx context.Context, tx pgx.Tx, companyID int64, jobs []fetcher.Job) ([]newJob, error) {
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
		RETURNING id, title, location`,
		companyID, ids, titles, locations, urls)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[newJob])
}

// queueNotifications matches new jobs against every active user's subscriptions
// and writes a pending notification per (user, job) match. This is the outbox:
// nothing is sent here, the notifier delivers pending rows after commit.
func queueNotifications(ctx context.Context, tx pgx.Tx, jobs []newJob) (int, error) {
	// shortcut: loads every subscription on each crawl with new jobs; fine for
	// hundreds of users, cache them or index keywords -> subscriptions beyond that.
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.user_id, s.include_keywords, s.exclude_keywords, s.locations
		FROM subscriptions s JOIN users u ON u.id = s.user_id
		WHERE u.active`)
	if err != nil {
		return 0, err
	}
	subs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Subscription])
	if err != nil {
		return 0, err
	}

	// Matching lives in Go, not SQL, so the aliases and whole-word rules exist once.
	var userIDs, jobIDs []int64
	for _, sub := range subs {
		m := match.Compile(match.Filter{Include: sub.IncludeKeywords, Exclude: sub.ExcludeKeywords, Locations: sub.Locations})
		for _, j := range jobs {
			if m.Match(j.Title, j.Location) {
				userIDs = append(userIDs, sub.UserID)
				jobIDs = append(jobIDs, j.ID)
			}
		}
	}
	if len(userIDs) == 0 {
		return 0, nil
	}

	// Two subscriptions of one user can match the same job: UNIQUE (user_id, job_id)
	// keeps one row, and RowsAffected counts only the rows actually queued.
	tag, err := tx.Exec(ctx, `
		INSERT INTO notifications (user_id, job_id)
		SELECT * FROM unnest($1::bigint[], $2::bigint[])
		ON CONFLICT (user_id, job_id) DO NOTHING`,
		userIDs, jobIDs)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
