package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var leverBaseURL = "https://api.lever.co/v0/postings"

// leverPosting is one element of Lever's response, which is a bare JSON array.
type leverPosting struct {
	ID         string `json:"id"`
	Text       string `json:"text"` // the job title
	HostedURL  string `json:"hostedUrl"`
	CreatedAt  int64  `json:"createdAt"` // Unix milliseconds, not a date string
	Categories struct {
		Location     string   `json:"location"`
		AllLocations []string `json:"allLocations"`
	} `json:"categories"`
}

// FetchLever returns every open job on a company's Lever board,
// e.g. company "meesho" -> https://api.lever.co/v0/postings/meesho?mode=json
func FetchLever(ctx context.Context, client *http.Client, company string) ([]Job, error) {
	var postings []leverPosting
	if err := getJSON(ctx, client, fmt.Sprintf("%s/%s?mode=json", leverBaseURL, company), &postings); err != nil {
		return nil, fmt.Errorf("lever %s: %w", company, err)
	}

	jobs := make([]Job, 0, len(postings))
	for _, p := range postings {
		loc := p.Categories.Location
		if len(p.Categories.AllLocations) > 0 {
			loc = strings.Join(p.Categories.AllLocations, "; ")
		}
		jobs = append(jobs, Job{
			ID:        p.ID,
			Company:   company, // Lever doesn't send a display name
			Title:     p.Text,
			Location:  loc,
			URL:       p.HostedURL,
			UpdatedAt: time.UnixMilli(p.CreatedAt),
		})
	}
	return jobs, nil
}
