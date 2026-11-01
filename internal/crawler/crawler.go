// Package crawler polls the boards that are due and records what changed.
package crawler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/store"
)

const (
	tick         = 5 * time.Second  // how often we ask the database what's due
	batchSize    = 10               // companies claimed per tick
	lease        = 5 * time.Minute  // a claimed company stays ours this long, even if we crash
	fetchTimeout = 20 * time.Second // one board must answer within this
)

type Crawler struct {
	Store  *store.Store
	Client *http.Client
	Log    *slog.Logger
}

// Run crawls due companies until ctx is cancelled.
func (c *Crawler) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		c.crawlDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Crawler) crawlDue(ctx context.Context) {
	companies, err := c.Store.ClaimDueCompanies(ctx, batchSize, lease)
	if err != nil {
		if ctx.Err() == nil {
			c.Log.Error("claim due companies", "err", err)
		}
		return
	}
	for _, co := range companies {
		if ctx.Err() != nil {
			return // shutting down; unfinished companies come back when their lease runs out
		}
		c.crawl(ctx, co)
	}
}

func (c *Crawler) crawl(ctx context.Context, co store.Company) {
	start := time.Now()
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	jobs, err := fetch(fetchCtx, c.Client, co)
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
	switch co.ATS {
	case "greenhouse":
		return fetcher.FetchGreenhouse(ctx, client, co.Slug)
	default:
		return nil, fmt.Errorf("unsupported ATS %q", co.ATS)
	}
}
