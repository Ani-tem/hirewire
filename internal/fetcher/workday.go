package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

const (
	workdayPageSize  = 20 // the most Workday returns per request
	workdayPages     = 5  // so we read the newest 100 jobs, not the whole board
	workdayDetailers = 4  // job-detail requests in flight at once
)

// workdayOrigin is where a tenant's board lives; tests point it at a local server.
var workdayOrigin = func(host string) string { return "https://" + host + ".myworkdayjobs.com" }

// Jobs open in several places are listed as "2 Locations"; only the job's detail page says which.
var multiLocation = regexp.MustCompile(`^\d+ Locations$`)

// workdayLocations caches the detail-page locations by job URL. A job's locations
// don't change, so each multi-location job costs one detail request per process.
// shortcut: never evicted; it grows by the new multi-location Workday jobs, a few hundred a day.
var workdayLocations sync.Map

type workdayResponse struct {
	JobPostings []struct {
		Title         string `json:"title"`
		ExternalPath  string `json:"externalPath"` // "/job/India-Bengaluru/Software-Engineer_JR123"
		LocationsText string `json:"locationsText"`
	} `json:"jobPostings"`
}

type workdayDetail struct {
	Info struct {
		Location            string   `json:"location"`
		AdditionalLocations []string `json:"additionalLocations"`
	} `json:"jobPostingInfo"`
}

// FetchWorkday returns the newest jobs on a Workday board. board is
// "<tenant>.<wd host>/<site>", from the careers URL, e.g. "nvidia.wd5/NVIDIAExternalCareerSite"
// for https://nvidia.wd5.myworkdayjobs.com/NVIDIAExternalCareerSite.
//
// Unlike the other fetchers this does not return the whole board: Workday serves
// 20 jobs a request at 1-2s each, so NVIDIA's 2000 would take minutes. It lists
// newest first, and we only need what was posted since the last crawl, so the
// newest 100 are plenty.
func FetchWorkday(ctx context.Context, client *http.Client, board string) ([]Job, error) {
	host, site, ok := strings.Cut(board, "/")
	tenant, _, _ := strings.Cut(host, ".")
	if !ok || site == "" || tenant == "" {
		return nil, fmt.Errorf("workday %s: board must look like tenant.wd5/site", board)
	}
	origin := workdayOrigin(host)
	api := fmt.Sprintf("%s/wday/cxs/%s/%s", origin, tenant, site)

	var jobs []Job
	var multi []int // indexes into jobs that need a detail lookup
	seen := map[string]bool{}
	for page := range workdayPages {
		var body workdayResponse
		err := postJSON(ctx, client, api+"/jobs", map[string]any{
			"appliedFacets": map[string]any{},
			"limit":         workdayPageSize,
			"offset":        page * workdayPageSize,
			"searchText":    "",
		}, &body)
		if err != nil {
			return nil, fmt.Errorf("workday %s: %w", board, err)
		}
		for _, p := range body.JobPostings {
			// A job posted between two page requests shifts the list, so one can appear twice.
			if seen[p.ExternalPath] {
				continue
			}
			seen[p.ExternalPath] = true
			if multiLocation.MatchString(p.LocationsText) {
				multi = append(multi, len(jobs))
			}
			jobs = append(jobs, Job{
				ID:       p.ExternalPath,
				Company:  tenant,
				Title:    p.Title,
				Location: p.LocationsText,
				URL:      origin + "/" + site + p.ExternalPath,
			})
		}
		if len(body.JobPostings) < workdayPageSize {
			break // last page
		}
	}

	// Each goroutine writes only its own jobs[i] and errs[n], so no lock is needed.
	errs := make([]error, len(multi))
	sem := make(chan struct{}, workdayDetailers)
	var wg sync.WaitGroup
	for n, i := range multi {
		j := &jobs[i]
		if loc, ok := workdayLocations.Load(j.URL); ok {
			j.Location = loc.(string)
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var d workdayDetail
			if err := getJSON(ctx, client, api+j.ID, &d); err != nil {
				errs[n] = err
				return
			}
			loc := strings.Join(append([]string{d.Info.Location}, d.Info.AdditionalLocations...), "; ")
			workdayLocations.Store(j.URL, loc)
			j.Location = loc
		})
	}
	wg.Wait()
	// Fail the crawl rather than save "2 Locations": a saved job is never updated,
	// so it would miss every location filter for good. The retry finds most details cached.
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("workday %s: job details: %w", board, err)
	}
	return jobs, nil
}
