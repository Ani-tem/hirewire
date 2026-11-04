package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	for _, ats := range []string{"greenhouse", "lever", "ashby", "workday"} {
		if Sources[ats] == nil {
			t.Errorf("no fetcher registered for %q", ats)
		}
	}
}

func TestFetchWorkday(t *testing.T) {
	// 25 jobs: a full page of 20, then 5 (the last page). Job 3 is posted in two
	// places, and job 20 shows up on both pages as if a new posting shifted the list.
	var details atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wday/cxs/acme/External/jobs":
			var req struct{ Offset int }
			json.NewDecoder(r.Body).Decode(&req)
			var ps []string
			from, to := req.Offset, min(req.Offset+20, 25)
			if req.Offset > 0 {
				from-- // repeat the last job of the previous page
			}
			for i := from; i < to; i++ {
				loc := "India, Bengaluru"
				if i == 3 {
					loc = "2 Locations"
				}
				ps = append(ps, fmt.Sprintf(`{"title":"Engineer %d","externalPath":"/job/x/Engineer_%d","locationsText":%q}`, i, i, loc))
			}
			fmt.Fprintf(w, `{"total":25,"jobPostings":[%s]}`, strings.Join(ps, ","))
		case r.Method == http.MethodGet && r.URL.Path == "/wday/cxs/acme/External/job/x/Engineer_3":
			details.Add(1)
			w.Write([]byte(`{"jobPostingInfo":{"location":"USA, Austin","additionalLocations":["India, Hyderabad"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	workdayOrigin = func(string) string { return srv.URL }

	for range 2 { // the second crawl must find job 3's locations in the cache
		jobs, err := FetchWorkday(context.Background(), srv.Client(), "acme.wd5/External")
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 25 {
			t.Fatalf("got %d jobs, want 25 (no duplicates)", len(jobs))
		}
		j := jobs[3]
		if j.Location != "USA, Austin; India, Hyderabad" {
			t.Errorf("multi-location job: location %q, want both places", j.Location)
		}
		if j.ID != "/job/x/Engineer_3" || j.URL != srv.URL+"/External/job/x/Engineer_3" || j.Company != "acme" {
			t.Errorf("bad mapping: %+v", j)
		}
	}
	if n := details.Load(); n != 1 {
		t.Errorf("fetched job 3's details %d times, want once", n)
	}
}

func TestFetchWorkdayRejectsBadBoard(t *testing.T) {
	if _, err := FetchWorkday(context.Background(), http.DefaultClient, "nvidia"); err == nil {
		t.Error("want an error for a board without a site")
	}
}
