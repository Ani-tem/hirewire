package crawler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/store"
)

// fakeStore hands out a fixed list of due companies and counts how often each is recorded.
type fakeStore struct {
	mu      sync.Mutex
	due     []store.Company
	crawled map[int64]int
	want    int
	allDone chan struct{} // closed once every company has been recorded
	once    sync.Once
}

func newFakeStore(n int) *fakeStore {
	f := &fakeStore{crawled: map[int64]int{}, want: n, allDone: make(chan struct{})}
	for i := range n {
		f.due = append(f.due, store.Company{ID: int64(i + 1), Name: "co", ATS: "greenhouse"})
	}
	return f
}

func (f *fakeStore) ClaimDueCompanies(_ context.Context, limit int, _ time.Duration) ([]store.Company, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := min(limit, len(f.due))
	claimed := f.due[:n]
	f.due = f.due[n:]
	return claimed, nil
}

func (f *fakeStore) RecordCrawl(_ context.Context, id int64, _ []fetcher.Job) (store.CrawlResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crawled[id]++
	if len(f.crawled) == f.want {
		f.once.Do(func() { close(f.allDone) })
	}
	return store.CrawlResult{}, nil
}

func (f *fakeStore) RecordFailure(context.Context, int64) error { return nil }

func TestRunCrawlsInParallel(t *testing.T) {
	const companies, workers = 20, 5
	const fetchTime = 50 * time.Millisecond

	f := newFakeStore(companies)
	var mu sync.Mutex
	inFlight, peak := 0, 0
	c := &Crawler{
		Store:   f,
		Workers: workers,
		Log:     slog.New(slog.DiscardHandler),
		Fetch: func(ctx context.Context, co store.Company) ([]fetcher.Job, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(fetchTime) // stands in for the network round trip
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		c.Run(ctx)
		close(stopped)
	}()

	select {
	case <-f.allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("not every company was crawled")
	}
	elapsed := time.Since(start)

	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}

	if peak != workers {
		t.Errorf("peak concurrency %d, want %d", peak, workers)
	}
	for id, n := range f.crawled {
		if n != 1 {
			t.Errorf("company %d crawled %d times, want 1", id, n)
		}
	}
	// One at a time, 20 boards × 50ms is 1s. Five workers should need about 200ms.
	if elapsed > 600*time.Millisecond {
		t.Errorf("took %v, want well under the 1s a single worker needs", elapsed)
	}
}
