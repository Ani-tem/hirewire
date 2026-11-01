package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
)

// testStore connects to the local docker compose Postgres (or $DATABASE_URL).
// It fails rather than skips without a database, so a broken setup can't pass silently.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://hirewire:hirewire@localhost:5433/hirewire?sslmode=disable"
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("no database (run `docker compose up -d`): %v", err)
	}
	t.Cleanup(s.Close)
	return s
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
