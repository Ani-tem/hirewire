package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGreenhouse(t *testing.T) {
	// A fake Greenhouse: real HTTP, canned response, no network needed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acme/jobs":
			w.Write([]byte(`{"jobs":[{"id":42,"title":"Software Engineer, New Grad",
				"absolute_url":"https://acme.com/jobs/42","company_name":"Acme",
				"updated_at":"2026-10-29T10:42:09-04:00","location":{"name":"Bengaluru"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	greenhouseBaseURL = srv.URL

	jobs, err := FetchGreenhouse(context.Background(), srv.Client(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.ID != "42" || j.Company != "Acme" || j.Location != "Bengaluru" || j.UpdatedAt.IsZero() {
		t.Errorf("bad mapping: %+v", j)
	}

	// A board that doesn't exist must return an error, not an empty list.
	if _, err := FetchGreenhouse(context.Background(), srv.Client(), "nope"); err == nil {
		t.Error("expected error for unknown board")
	}
}
