package store

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/jackc/pgx/v5"
)

// testStore connects to a separate hirewire_test database (or $TEST_DATABASE_URL),
// creating it on first use, so tests can wipe tables without touching dev data.
// It fails rather than skips without a database, so a broken setup can't pass silently.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://hirewire:hirewire@localhost:5433/hirewire_test?sslmode=disable"
		admin, err := pgx.Connect(ctx, "postgres://hirewire:hirewire@localhost:5433/hirewire?sslmode=disable")
		if err != nil {
			t.Fatalf("no database (run `docker compose up -d`): %v", err)
		}
		var exists bool
		admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'hirewire_test')`).Scan(&exists)
		if !exists {
			if _, err := admin.Exec(ctx, `CREATE DATABASE hirewire_test`); err != nil {
				t.Fatalf("create test database: %v", err)
			}
		}
		admin.Close(ctx)
	}
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("no database (run `docker compose up -d`): %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// reset empties every table, for tests that care about the whole companies table.
func reset(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE companies CASCADE`); err != nil {
		t.Fatal(err)
	}
}

func TestAddCompanyIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	slug := "test-" + strconv.FormatInt(time.Now().UnixNano(), 36) // unique, so reruns don't collide
	t.Cleanup(func() { s.pool.Exec(ctx, `DELETE FROM companies WHERE slug = $1`, slug) })

	// Add the same board three times; the second run fixes a typo in the name.
	for _, name := range []string{"Typo Crop", "Real Corp", "Real Corp"} {
		if err := s.AddCompany(ctx, name, "greenhouse", slug); err != nil {
			t.Fatal(err)
		}
	}

	var count int
	var name string
	err := s.pool.QueryRow(ctx,
		`SELECT count(*), max(name) FROM companies WHERE ats = 'greenhouse' AND slug = $1`, slug,
	).Scan(&count, &name)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("got %d rows, want exactly 1", count)
	}
	if name != "Real Corp" {
		t.Errorf("name = %q, want the latest name %q", name, "Real Corp")
	}
}

func TestAddCompanyRejectsUnknownATS(t *testing.T) {
	s := testStore(t)
	// The schema's CHECK constraint only allows greenhouse, lever, ashby.
	if err := s.AddCompany(context.Background(), "X", "workday", "x"); err == nil {
		t.Error("expected an error for an unsupported ATS")
	}
}

func TestRecordCrawl(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	slug := "test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := s.AddCompany(ctx, "Acme", "greenhouse", slug); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", slug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM jobs WHERE company_id = $1`, c.ID)
		s.pool.Exec(ctx, `DELETE FROM companies WHERE id = $1`, c.ID)
	})

	job := func(id string) fetcher.Job {
		return fetcher.Job{ID: id, Title: "Engineer " + id, Location: "Bengaluru", URL: "https://x/" + id}
	}

	crawls := []struct {
		name         string
		board        []fetcher.Job
		wantNew      int
		wantBaseline bool
	}{
		{"first crawl is the baseline", []fetcher.Job{job("1"), job("2")}, 2, true},
		{"job 3 is posted", []fetcher.Job{job("1"), job("2"), job("3")}, 1, false},
		{"nothing changed", []fetcher.Job{job("1"), job("2"), job("3")}, 0, false},
	}
	for _, cr := range crawls {
		got, err := s.RecordCrawl(ctx, c.ID, cr.board)
		if err != nil {
			t.Fatalf("%s: %v", cr.name, err)
		}
		if len(got.NewJobIDs) != cr.wantNew || got.Baseline != cr.wantBaseline {
			t.Errorf("%s: got %d new, baseline=%v; want %d new, baseline=%v",
				cr.name, len(got.NewJobIDs), got.Baseline, cr.wantNew, cr.wantBaseline)
		}
	}
}

func TestClaimDueCompaniesHandsEachCompanyOutOnce(t *testing.T) {
	s := testStore(t)
	reset(t, s)
	ctx := context.Background()
	for i := range 20 {
		if err := s.AddCompany(ctx, "Co", "greenhouse", "co-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Eight workers claim at the same moment. Every company must go to exactly one of them.
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimDueCompanies(ctx, 5, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, c := range got {
				seen[c.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != 20 {
		t.Errorf("claimed %d distinct companies, want all 20", len(seen))
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("company %d was claimed %d times", id, n)
		}
	}

	// Everything is leased now, so nothing is due.
	got, err := s.ClaimDueCompanies(ctx, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("claimed %d companies while all are leased, want 0", len(got))
	}
}

func TestRecordFailureBacksOff(t *testing.T) {
	s := testStore(t)
	reset(t, s)
	ctx := context.Background()
	if err := s.AddCompany(ctx, "Flaky", "greenhouse", "flaky"); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", "flaky")
	if err != nil {
		t.Fatal(err)
	}

	// poll_interval is 15 minutes, so failures wait 30m, 1h, 2h, 4h, then stay at the 6h cap.
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if err := s.RecordFailure(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		var wait float64
		err := s.pool.QueryRow(ctx,
			`SELECT EXTRACT(EPOCH FROM next_poll_at - now()) FROM companies WHERE id = $1`, c.ID).Scan(&wait)
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Duration(wait) * time.Second; got.Round(time.Minute) != w {
			t.Errorf("failure %d: next poll in %v, want %v", i+1, got.Round(time.Minute), w)
		}
	}
}
