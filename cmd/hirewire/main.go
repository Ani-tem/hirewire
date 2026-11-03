package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ani-tem/hirewire/internal/crawler"
	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/match"
	"github.com/Ani-tem/hirewire/internal/store"
)

const usage = `usage: hirewire <command>

  run           crawl every company on schedule until Ctrl-C
  seed          add every company in data/companies.csv to the database
  try [flags]   show which stored jobs a subscription would match, e.g.
                try -include "backend,sde" -exclude "senior,staff" -location "bengaluru,remote"
  crawl <slug>  fetch one Greenhouse board, save it, report how many jobs are new
  fetch  print Stripe's engineering roles (smoke test, no database)`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		run()
	case "seed":
		seed()
	case "try":
		try(os.Args[2:])
	case "crawl":
		if len(os.Args) < 3 {
			fmt.Println(usage)
			os.Exit(2)
		}
		crawl(os.Args[2])
	case "fetch":
		fetchDemo()
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
}

func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://hirewire:hirewire@localhost:5433/hirewire?sslmode=disable"
}

func run() {
	// Ctrl-C (or SIGTERM from a deploy) cancels ctx, and the crawler stops after its current board.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := store.Open(ctx, databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	const workers = 8
	// Every Greenhouse board is on one host. Go's default keeps only 2 idle
	// connections per host, so 8 workers would keep reconnecting (new TCP + TLS each time).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = workers
	client := &http.Client{Timeout: 90 * time.Second, Transport: transport} // backstop above the crawler's per-board timeout

	c := crawler.New(s, client, workers, slog.Default())
	slog.Info("hirewire running, Ctrl-C to stop", "workers", workers)
	c.Run(ctx)
	slog.Info("stopped")
}

func try(args []string) {
	fs := flag.NewFlagSet("try", flag.ExitOnError)
	include := fs.String("include", "", "comma-separated title keywords; at least one must appear")
	exclude := fs.String("exclude", "", "comma-separated title keywords; none may appear")
	location := fs.String("location", "", "comma-separated locations; at least one must appear")
	fs.Parse(args)

	ctx := context.Background()
	s, err := store.Open(ctx, databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		log.Fatal(err)
	}

	m := match.Compile(match.Filter{Include: list(*include), Exclude: list(*exclude), Locations: list(*location)})
	matched := 0
	for _, j := range jobs {
		if !m.Match(j.Title, j.Location) {
			continue
		}
		if matched < 25 {
			fmt.Printf("%-12.12s | %-50.50s | %-28.28s\n", j.Company, j.Title, j.Location)
		}
		matched++
	}
	fmt.Printf("\n%d of %d jobs match\n", matched, len(jobs))
}

// list splits a comma-separated flag value.
func list(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func seed() {
	ctx := context.Background()
	s, err := store.Open(ctx, databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	f, err := os.Open("data/companies.csv")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		log.Fatal(err)
	}

	for _, r := range rows[1:] { // skip the header row
		name, ats, slug := r[0], r[1], r[2]
		if err := s.AddCompany(ctx, name, ats, slug); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
	}
	fmt.Printf("seeded %d companies\n", len(rows)-1)
}

func crawl(slug string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx, databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	c, err := s.GetCompany(ctx, "greenhouse", slug)
	if err != nil {
		log.Fatal(err)
	}
	jobs, err := fetcher.FetchGreenhouse(ctx, &http.Client{Timeout: 10 * time.Second}, slug)
	if err != nil {
		log.Fatal(err)
	}
	res, err := s.RecordCrawl(ctx, c.ID, jobs)
	if err != nil {
		log.Fatal(err)
	}
	if res.Baseline {
		fmt.Printf("%s: baseline saved, %d jobs already on the board (no alerts)\n", c.Name, len(res.NewJobIDs))
		return
	}
	fmt.Printf("%s: %d jobs on the board, %d new, %d notifications queued\n", c.Name, len(jobs), len(res.NewJobIDs), res.Notified)
}

func fetchDemo() {
	// Never call the network without a timeout: a hung server would hang us forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second}

	jobs, err := fetcher.FetchGreenhouse(ctx, client, "stripe")
	if err != nil {
		log.Fatal(err)
	}

	matched := 0
	for _, j := range jobs {
		if strings.Contains(strings.ToLower(j.Title), "engineer") {
			fmt.Printf("%-55s | %-35s | %s\n", j.Title, j.Location, j.URL)
			matched++
		}
	}
	fmt.Printf("\n%d engineering roles out of %d total\n", matched, len(jobs))
}
