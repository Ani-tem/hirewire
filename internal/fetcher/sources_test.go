package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeBoard serves body at path and 404s everything else.
func fakeBoard(t *testing.T, path, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchLever(t *testing.T) {
	srv := fakeBoard(t, "/meesho", `[{"id":"abc-123","text":"SDE II, Backend",
		"hostedUrl":"https://jobs.lever.co/meesho/abc-123","createdAt":1786469891368,
		"categories":{"location":"Bangalore, Karnataka","allLocations":["Bangalore, Karnataka","Remote, India"]}}]`)
	leverBaseURL = srv.URL

	jobs, err := FetchLever(context.Background(), srv.Client(), "meesho")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.ID != "abc-123" || j.Title != "SDE II, Backend" || j.URL != "https://jobs.lever.co/meesho/abc-123" {
		t.Errorf("bad mapping: %+v", j)
	}
	if j.Location != "Bangalore, Karnataka; Remote, India" {
		t.Errorf("location = %q, want every location joined", j.Location)
	}
	// createdAt is milliseconds; reading it as seconds would land in the year ~58,000.
	if want := time.UnixMilli(1786469891368); !j.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", j.UpdatedAt, want)
	}

	if _, err := FetchLever(context.Background(), srv.Client(), "nope"); err == nil {
		t.Error("expected error for unknown company")
	}
}

func TestFetchAshby(t *testing.T) {
	srv := fakeBoard(t, "/composio", `{"jobs":[
		{"id":"j1","title":"Founding Engineer","location":"SF",
		 "secondaryLocations":[{"location":"Bangalore"}],
		 "jobUrl":"https://jobs.ashbyhq.com/composio/j1","publishedAt":"2026-08-24T14:44:49.699+00:00","isListed":true},
		{"id":"j2","title":"Secret Role","location":"SF","jobUrl":"https://x","publishedAt":"2026-08-24T14:44:49Z","isListed":false}]}`)
	ashbyBaseURL = srv.URL

	jobs, err := FetchAshby(context.Background(), srv.Client(), "composio")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1 (unlisted jobs are skipped)", len(jobs))
	}
	j := jobs[0]
	if j.ID != "j1" || j.Title != "Founding Engineer" || j.UpdatedAt.IsZero() {
		t.Errorf("bad mapping: %+v", j)
	}
	if j.Location != "SF; Bangalore" {
		t.Errorf("location = %q, want the secondary location kept", j.Location)
	}

	if _, err := FetchAshby(context.Background(), srv.Client(), "nope"); err == nil {
		t.Error("expected error for unknown board")
	}
}

func TestEveryAllowedATSHasAFetcher(t *testing.T) {
	// Mirrors the CHECK constraint on companies.ats in store/schema.sql.
	for _, ats := range []string{"greenhouse", "lever", "ashby"} {
		if Sources[ats] == nil {
			t.Errorf("no fetcher registered for %q", ats)
		}
	}
}
