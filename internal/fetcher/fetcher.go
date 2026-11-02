package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
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
}

// getJSON GETs url and decodes a 200 response body into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
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
