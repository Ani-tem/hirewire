package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Func fetches every open job on one company's board. slug is the company's
// name in that ATS's URLs, e.g. "stripe" or "meesho".
type Func func(ctx context.Context, client *http.Client, slug string) ([]Job, error)

// Sources maps an ATS name, as stored in companies.ats, to its fetcher.
// Supporting a new ATS means one new file and one line here.
var Sources = map[string]Func{
	"greenhouse": FetchGreenhouse,
	"lever":      FetchLever,
	"ashby":      FetchAshby,
	"workday":    FetchWorkday,
}

// getJSON GETs url and decodes a 200 response body into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	return doJSON(ctx, client, http.MethodGet, url, nil, v)
}

// postJSON POSTs body as JSON to url and decodes a 200 response body into v.
func postJSON(ctx context.Context, client *http.Client, url string, body, v any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return doJSON(ctx, client, http.MethodPost, url, bytes.NewReader(b), v)
}

func doJSON(ctx context.Context, client *http.Client, method, url string, body io.Reader, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// A 404 or 500 is not a Go error; client.Do only fails on network problems.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
