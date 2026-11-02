package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var ashbyBaseURL = "https://api.ashbyhq.com/posting-api/job-board"

type ashbyResponse struct {
	Jobs []struct {
		ID                 string `json:"id"`
		Title              string `json:"title"`
		Location           string `json:"location"`
		SecondaryLocations []struct {
			Location string `json:"location"`
		} `json:"secondaryLocations"`
		JobURL      string    `json:"jobUrl"`
		PublishedAt time.Time `json:"publishedAt"`
		IsListed    bool      `json:"isListed"`
	} `json:"jobs"`
}

// FetchAshby returns every listed job on a company's Ashby board,
// e.g. board "sarvam" -> https://api.ashbyhq.com/posting-api/job-board/sarvam
func FetchAshby(ctx context.Context, client *http.Client, board string) ([]Job, error) {
	var body ashbyResponse
	if err := getJSON(ctx, client, fmt.Sprintf("%s/%s", ashbyBaseURL, board), &body); err != nil {
		return nil, fmt.Errorf("ashby %s: %w", board, err)
	}

	jobs := make([]Job, 0, len(body.Jobs))
	for _, j := range body.Jobs {
		if !j.IsListed {
			continue // hidden from the public board, so not something a user can apply to
		}
		// Keep every location: a job based in SF may also be open in Bangalore.
		locs := []string{j.Location}
		for _, s := range j.SecondaryLocations {
			locs = append(locs, s.Location)
		}
		jobs = append(jobs, Job{
			ID:        j.ID,
			Company:   board, // Ashby doesn't send a display name
			Title:     j.Title,
			Location:  strings.Join(locs, "; "),
			URL:       j.JobURL,
			UpdatedAt: j.PublishedAt,
		})
	}
	return jobs, nil
}
