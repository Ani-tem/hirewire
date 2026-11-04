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
	"sync"
	"syscall"
	"time"

	"github.com/Ani-tem/hirewire/data"
	"github.com/Ani-tem/hirewire/internal/bot"
	"github.com/Ani-tem/hirewire/internal/crawler"
	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/match"
	"github.com/Ani-tem/hirewire/internal/notifier"
	"github.com/Ani-tem/hirewire/internal/store"
	"github.com/Ani-tem/hirewire/internal/telegram"
)

const usage = `usage: hirewire <command>

  run           crawl on schedule, answer the bot and send alerts until Ctrl-C
                (without TELEGRAM_BOT_TOKEN it only crawls)
  seed          add every company in data/companies.csv to the database
  try [flags]   show which stored jobs a subscription would match, e.g.
                try -include "backend,sde" -exclude "senior,staff" -location "bengaluru,remote"
  crawl <slug>  fetch one Greenhouse board, save it, report how many jobs are new
  fetch  print Stripe's engineering roles (smoke test, no database)`

func main() {
	if os.Getenv("LOG_FORMAT") == "json" { // in production, one JSON object per line for log tools
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
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
	// Ctrl-C (or SIGTERM from a deploy) cancels ctx; each part finishes what it is doing and returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := store.Open(ctx, databaseURL())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	n, err := seedCompanies(ctx, s)
	if err != nil {
		log.Fatal(err)
	}
	slog.Info("companies seeded", "count", n)

	const workers = 8
	// Every Greenhouse board is on one host. Go's default keeps only 2 idle
	// connections per host, so 8 workers would keep reconnecting (new TCP + TLS each time).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = workers
	client := &http.Client{Timeout: 90 * time.Second, Transport: transport} // backstop above the crawler's per-board timeout

	// One process, three loops sharing ctx: simple to deploy, and they only talk through Postgres.
	var wg sync.WaitGroup
	wg.Go(func() { crawler.New(s, client, workers, slog.Default()).Run(ctx) })

	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		// The timeout must outlast a long poll, which Telegram holds open for up to 30s.
		tg := telegram.New(token, &http.Client{Timeout: 60 * time.Second})
		wg.Go(func() { (&bot.Bot{Store: s, TG: tg, Log: slog.Default()}).Run(ctx) })
		wg.Go(func() { notifier.New(s, tg, slog.Default()).Run(ctx) })
	} else {
		slog.Warn("TELEGRAM_BOT_TOKEN not set: crawling only, no bot and no alerts")
	}
	if url := os.Getenv("HEALTHCHECK_URL"); url != "" {
		wg.Go(func() { heartbeat(ctx, s, url) })
	}

	slog.Info("hirewire running, Ctrl-C to stop", "workers", workers)
	wg.Wait()
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
	n, err := seedCompanies(ctx, s)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("seeded %d companies\n", n)
}

// seedCompanies adds every company in the embedded companies.csv. AddCompany is
// an upsert, so this is safe on every start and picks up companies added since.
func seedCompanies(ctx context.Context, s *store.Store) (int, error) {
	rows, err := csv.NewReader(strings.NewReader(data.CompaniesCSV)).ReadAll()
	if err != nil {
		return 0, fmt.Errorf("read companies.csv: %w", err)
	}
	for _, r := range rows[1:] { // skip the header row
		if err := s.AddCompany(ctx, r[0], r[1], r[2]); err != nil {
			return 0, err
		}
	}
	return len(rows) - 1, nil
}

// heartbeat pings url every few minutes while crawls are succeeding. The
// monitor (healthchecks.io) emails us when pings stop: that catches a crash,
// a hang and a crawler that runs but fails every board, which a plain
// "process is up" check would miss.
func heartbeat(ctx context.Context, s *store.Store, url string) {
	const every, stale = 5 * time.Minute, 30 * time.Minute
	client := &http.Client{Timeout: 10 * time.Second}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		last, err := s.LastCrawlSuccess(ctx)
		if err != nil || time.Since(last) > stale {
			slog.Warn("no successful crawl recently, skipping heartbeat", "last", last, "err", err)
			continue
		}
		if resp, err := client.Get(url); err != nil {
			slog.Warn("heartbeat failed", "err", err)
		} else {
			resp.Body.Close()
		}
	}
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
