// Package fetcher pulls job postings from public ATS (applicant tracking system) APIs.
package fetcher

import "time"

// Job is one posting, normalized so every source (Greenhouse, Lever, ...) looks the same.
type Job struct {
	ID        string // the source's own ID, unique within one company board
	Company   string
	Title     string
	Location  string
	URL       string
	UpdatedAt time.Time
}
