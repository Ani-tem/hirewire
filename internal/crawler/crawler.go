// Package crawler polls the boards that are due and records what changed.
package crawler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/store"
)

const (
	tick         = 5 * time.Second  // how often we ask the database what's due, when nothing is waiting
	lease        = 5 * time.Minute  // a claimed company stays ours this long, even if we crash
	fetchTimeout = 60 * time.Second // Lever sends full descriptions at ~200 KB/s; Paytm alone is 3 MB
)

// Store is what the crawler needs from the database. *store.Store satisfies it;
// tests pass an in-memory fake.
type Store interface {
	ClaimDueCompanies(ctx context.Context, limit int, lease time.Duration) ([]store.Company, error)
	RecordCrawl(ctx context.Context, companyID int64, jobs []fetcher.Job) (store.CrawlResult, error)
	RecordFailure(ctx context.Context, companyID int64) error
}

type Crawler struct {
	Store   Store
	Fetch   func(ctx context.Context, co store.Company) ([]fetcher.Job, error)
	Workers int // boards crawled at the same time
	Log     *slog.Logger
}

// New returns a crawler that fetches from the real job boards.
func New(s Store, client *http.Client, workers int, log *slog.Logger) *Crawler {
	return &Crawler{
		Store:   s,
		Workers: workers,
		Log:     log,
		Fetch: func(ctx context.Context, co store.Company) ([]fetcher.Job, error) {
			return fetch(ctx, client, co)
		},
	}
}

// Run crawls due companies on c.Workers goroutines until ctx is cancelled,
// and returns once every worker has exited.
func (c *Crawler) Run(ctx context.Context) {
	workers := max(c.Workers, 1)

	// Unbuffered: a send completes only when a worker is free to take the board,
	// so the dispatcher can never run ahead of the workers.
	work := make(chan store.Company)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for co := range work {
				c.crawl(ctx, co)
			}
		})
	}

	c.dispatch(ctx, work, workers)
	close(work) // ends every worker's range loop
	wg.Wait()
}

// dispatch claims due companies and hands them to the workers until ctx is cancelled.
func (c *Crawler) dispatch(ctx context.Context, work chan<- store.Company, batch int) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		companies, err := c.Store.ClaimDueCompanies(ctx, batch, lease)
		if err != nil && ctx.Err() == nil {
			c.Log.Error("claim due companies", "err", err)
		}
		for _, co := range companies {
			select {
			case work <- co:
			case <-ctx.Done():
				return // boards we claimed but never sent come back when their lease runs out
			}
		}
		if len(companies) == batch {
			continue // a full batch means more may be due: claim again without waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Crawler) crawl(ctx context.Context, co store.Company) {
	start := time.Now()
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	jobs, err := c.Fetch(fetchCtx, co)
	var res store.CrawlResult
	if err == nil {
		res, err = c.Store.RecordCrawl(ctx, co.ID, jobs)
	}
	if err != nil {
		if ctx.Err() != nil {
			return // interrupted by shutdown, not the board's fault: don't count a failure
		}
		c.Log.Warn("crawl failed", "company", co.Name, "err", err)
		if err := c.Store.RecordFailure(ctx, co.ID); err != nil {
			c.Log.Error("record failure", "company", co.Name, "err", err)
		}
		return
	}

	// TODO(step 7): match res.NewJobIDs against subscriptions instead of only logging them.
	c.Log.Info("crawled", "company", co.Name, "jobs", len(jobs), "new", len(res.NewJobIDs),
		"baseline", res.Baseline, "took", time.Since(start).Round(time.Millisecond))
}

func fetch(ctx context.Context, client *http.Client, co store.Company) ([]fetcher.Job, error) {
	f, ok := fetcher.Sources[co.ATS]
	if !ok {
		return nil, fmt.Errorf("unsupported ATS %q", co.ATS)
	}
	return f(ctx, client, co.Slug)
}
