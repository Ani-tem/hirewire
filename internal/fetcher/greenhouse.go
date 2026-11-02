package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A var, not a const, so tests can point it at a fake server.
var greenhouseBaseURL = "https://boards-api.greenhouse.io/v1/boards"

// greenhouseResponse mirrors only the JSON fields we use; encoding/json ignores the rest.
type greenhouseResponse struct {
	Jobs []struct {
		ID          int64     `json:"id"`
		Title       string    `json:"title"`
		AbsoluteURL string    `json:"absolute_url"`
		CompanyName string    `json:"company_name"`
		UpdatedAt   time.Time `json:"updated_at"`
		Location    struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"jobs"`
}

// FetchGreenhouse returns every open job on a company's Greenhouse board,
// e.g. board "stripe" -> https://boards-api.greenhouse.io/v1/boards/stripe/jobs
func FetchGreenhouse(ctx context.Context, client *http.Client, board string) ([]Job, error) {
	var body greenhouseResponse
	if err := getJSON(ctx, client, fmt.Sprintf("%s/%s/jobs", greenhouseBaseURL, board), &body); err != nil {
		return nil, fmt.Errorf("greenhouse %s: %w", board, err)
	}

	jobs := make([]Job, 0, len(body.Jobs))
	for _, j := range body.Jobs {
		jobs = append(jobs, Job{
			ID:        strconv.FormatInt(j.ID, 10),
			Company:   j.CompanyName,
			Title:     j.Title,
			Location:  j.Location.Name,
			URL:       j.AbsoluteURL,
			UpdatedAt: j.UpdatedAt,
		})
	}
	return jobs, nil
}
