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

func TestSaveJobsReturnsOnlyNewJobs(t *testing.T) {
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

	// First crawl: everything is new.
	got, err := s.SaveJobs(ctx, c.ID, []fetcher.Job{job("1"), job("2")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("first crawl: got %d new jobs, want 2", len(got))
	}

	// Second crawl: 1 and 2 are old, 3 was just posted.
	got, err = s.SaveJobs(ctx, c.ID, []fetcher.Job{job("1"), job("2"), job("3")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("second crawl: got %d new jobs, want 1", len(got))
	}

	// Same crawl again: nothing new.
	got, err = s.SaveJobs(ctx, c.ID, []fetcher.Job{job("1"), job("2"), job("3")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("repeat crawl: got %d new jobs, want 0", len(got))
	}
}
