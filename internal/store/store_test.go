package store

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
	"github.com/Ani-tem/hirewire/internal/match"
	"github.com/jackc/pgx/v5"
)

// testStore connects to a separate hirewire_test database (or $TEST_DATABASE_URL),
// creating it on first use, so tests can wipe tables without touching dev data.
// It fails rather than skips without a database, so a broken setup can't pass silently.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://hirewire:hirewire@localhost:5433/hirewire_test?sslmode=disable"
		admin, err := pgx.Connect(ctx, "postgres://hirewire:hirewire@localhost:5433/hirewire?sslmode=disable")
		if err != nil {
			t.Fatalf("no database (run `docker compose up -d`): %v", err)
		}
		var exists bool
		admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'hirewire_test')`).Scan(&exists)
		if !exists {
			if _, err := admin.Exec(ctx, `CREATE DATABASE hirewire_test`); err != nil {
				t.Fatalf("create test database: %v", err)
			}
		}
		admin.Close(ctx)
	}
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("no database (run `docker compose up -d`): %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// reset empties every table, for tests that care about the whole companies table.
func reset(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE companies, users CASCADE`); err != nil {
		t.Fatal(err)
	}
}

func TestAddCompanyIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	slug := "test-" + strconv.FormatInt(time.Now().UnixNano(), 36) // unique, so reruns don't collide
	t.Cleanup(func() { s.pool.Exec(ctx, `DELETE FROM companies WHERE slug = $1`, slug) })

	// Add the same board three times; the second run fixes a typo in the name.
	for _, name := range []string{"Typo Crop", "Real Corp", "Real Corp"} {
		if err := s.AddCompany(ctx, name, "greenhouse", slug); err != nil {
			t.Fatal(err)
		}
	}

	var count int
	var name string
	err := s.pool.QueryRow(ctx,
		`SELECT count(*), max(name) FROM companies WHERE ats = 'greenhouse' AND slug = $1`, slug,
	).Scan(&count, &name)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("got %d rows, want exactly 1", count)
	}
	if name != "Real Corp" {
		t.Errorf("name = %q, want the latest name %q", name, "Real Corp")
	}
}

func TestAddCompanyRejectsUnknownATS(t *testing.T) {
	s := testStore(t)
	// The schema's CHECK constraint only allows the ATSs we have fetchers for.
	if err := s.AddCompany(context.Background(), "X", "smartrecruiters", "x"); err == nil {
		t.Error("expected an error for an unsupported ATS")
	}
}

func TestRecordCrawl(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	slug := "test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := s.AddCompany(ctx, "Acme", "greenhouse", slug); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", slug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM jobs WHERE company_id = $1`, c.ID)
		s.pool.Exec(ctx, `DELETE FROM companies WHERE id = $1`, c.ID)
	})

	job := func(id string) fetcher.Job {
		return fetcher.Job{ID: id, Title: "Engineer " + id, Location: "Bengaluru", URL: "https://x/" + id}
	}

	crawls := []struct {
		name         string
		board        []fetcher.Job
		wantNew      int
		wantBaseline bool
	}{
		{"first crawl is the baseline", []fetcher.Job{job("1"), job("2")}, 2, true},
		{"job 3 is posted", []fetcher.Job{job("1"), job("2"), job("3")}, 1, false},
		{"nothing changed", []fetcher.Job{job("1"), job("2"), job("3")}, 0, false},
	}
	for _, cr := range crawls {
		got, err := s.RecordCrawl(ctx, c.ID, cr.board)
		if err != nil {
			t.Fatalf("%s: %v", cr.name, err)
		}
		if len(got.NewJobIDs) != cr.wantNew || got.Baseline != cr.wantBaseline {
			t.Errorf("%s: got %d new, baseline=%v; want %d new, baseline=%v",
				cr.name, len(got.NewJobIDs), got.Baseline, cr.wantNew, cr.wantBaseline)
		}
	}
}

func TestClaimDueCompaniesHandsEachCompanyOutOnce(t *testing.T) {
	s := testStore(t)
	reset(t, s)
	ctx := context.Background()
	for i := range 20 {
		if err := s.AddCompany(ctx, "Co", "greenhouse", "co-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Eight workers claim at the same moment. Every company must go to exactly one of them.
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimDueCompanies(ctx, 5, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, c := range got {
				seen[c.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != 20 {
		t.Errorf("claimed %d distinct companies, want all 20", len(seen))
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("company %d was claimed %d times", id, n)
		}
	}

	// Everything is leased now, so nothing is due.
	got, err := s.ClaimDueCompanies(ctx, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("claimed %d companies while all are leased, want 0", len(got))
	}
}

func TestRecordFailureBacksOff(t *testing.T) {
	s := testStore(t)
	reset(t, s)
	ctx := context.Background()
	if err := s.AddCompany(ctx, "Flaky", "greenhouse", "flaky"); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", "flaky")
	if err != nil {
		t.Fatal(err)
	}

	// poll_interval is 15 minutes, so failures wait 30m, 1h, 2h, 4h, then stay at the 6h cap.
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if err := s.RecordFailure(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		var wait float64
		err := s.pool.QueryRow(ctx,
			`SELECT EXTRACT(EPOCH FROM next_poll_at - now()) FROM companies WHERE id = $1`, c.ID).Scan(&wait)
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Duration(wait) * time.Second; got.Round(time.Minute) != w {
			t.Errorf("failure %d: next poll in %v, want %v", i+1, got.Round(time.Minute), w)
		}
	}
}

func TestRecordCrawlQueuesNotifications(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	reset(t, s)
	t.Cleanup(func() { reset(t, s) }) // leftover subscriptions would match other tests' jobs

	if err := s.AddCompany(ctx, "Razorpay", "greenhouse", "razorpay"); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", "razorpay")
	if err != nil {
		t.Fatal(err)
	}

	subscribe := func(chatID int64, f ...match.Filter) int64 {
		t.Helper()
		id, err := s.AddUser(ctx, chatID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range f {
			if _, err := s.AddSubscription(ctx, id, f); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	backend := match.Filter{Include: []string{"backend"}, Exclude: []string{"senior"}, Locations: []string{"bengaluru"}}
	// Ravi has two subscriptions that both match a Bengaluru backend job.
	ravi := subscribe(1, backend, match.Filter{Locations: []string{"bengaluru"}})
	priya := subscribe(2, match.Filter{Include: []string{"designer"}})
	gone := subscribe(3, backend)
	if _, err := s.pool.Exec(ctx, `UPDATE users SET active = false WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}

	job := func(id, title, loc string) fetcher.Job {
		return fetcher.Job{ID: id, Title: title, Location: loc, URL: "https://x/" + id}
	}
	old := job("1", "Backend Engineer", "Bangalore") // matches Ravi, but was already on the board

	crawls := []struct {
		name         string
		board        []fetcher.Job
		wantNotified int
	}{
		{"baseline queues nothing", []fetcher.Job{old}, 0},
		{"one job, two of ravi's subscriptions: one alert", []fetcher.Job{old, job("2", "Backend Engineer", "Bengaluru")}, 1},
		{"no one wants a senior SF role", []fetcher.Job{old, job("3", "Senior Backend Engineer", "San Francisco")}, 0},
		{"designer reaches priya only", []fetcher.Job{old, job("4", "Product Designer", "Remote")}, 1},
	}
	for _, cr := range crawls {
		got, err := s.RecordCrawl(ctx, c.ID, cr.board)
		if err != nil {
			t.Fatalf("%s: %v", cr.name, err)
		}
		if got.Notified != cr.wantNotified {
			t.Errorf("%s: notified %d, want %d", cr.name, got.Notified, cr.wantNotified)
		}
	}

	// The outbox itself: exactly these pending rows, and nothing for the inactive user.
	rows, err := s.pool.Query(ctx, `
		SELECT n.user_id, j.external_id FROM notifications n JOIN jobs j ON j.id = n.job_id
		WHERE n.status = 'pending' ORDER BY n.user_id, j.external_id`)
	if err != nil {
		t.Fatal(err)
	}
	type pair struct {
		UserID int64
		JobID  string
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pair])
	if err != nil {
		t.Fatal(err)
	}
	want := []pair{{ravi, "2"}, {priya, "4"}}
	if !slices.Equal(got, want) {
		t.Errorf("pending notifications = %v, want %v", got, want)
	}
}

func TestSubscriptionsBelongToTheirUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	reset(t, s)
	t.Cleanup(func() { reset(t, s) })

	ravi, err := s.AddUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	priya, err := s.AddUser(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.AddSubscription(ctx, ravi, match.Filter{Include: []string{"backend"}})
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := s.DeleteSubscription(ctx, priya, sub); err != nil || ok {
		t.Errorf("priya deleted ravi's subscription: ok=%v err=%v", ok, err)
	}
	subs, err := s.ListSubscriptions(ctx, ravi)
	if err != nil || len(subs) != 1 || !slices.Equal(subs[0].IncludeKeywords, []string{"backend"}) {
		t.Fatalf("ravi's subscriptions = %+v, %v; want his one backend subscription", subs, err)
	}
	if len(subs[0].ExcludeKeywords) != 0 || subs[0].ExcludeKeywords == nil {
		t.Errorf("unset excludes = %#v, want an empty array, not NULL", subs[0].ExcludeKeywords)
	}
	if ok, err := s.DeleteSubscription(ctx, ravi, sub); err != nil || !ok {
		t.Errorf("ravi could not delete his own subscription: ok=%v err=%v", ok, err)
	}
}

func TestNotificationLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	reset(t, s)
	t.Cleanup(func() { reset(t, s) })

	if err := s.AddCompany(ctx, "Razorpay", "greenhouse", "razorpay"); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCompany(ctx, "greenhouse", "razorpay")
	if err != nil {
		t.Fatal(err)
	}
	ravi, _ := s.AddUser(ctx, 100)
	priya, _ := s.AddUser(ctx, 200)
	for _, u := range []int64{ravi, priya} {
		if _, err := s.AddSubscription(ctx, u, match.Filter{Include: []string{"backend"}}); err != nil {
			t.Fatal(err)
		}
	}
	s.RecordCrawl(ctx, c.ID, nil) // baseline: empty board
	board := []fetcher.Job{{ID: "1", Title: "Backend Engineer", Location: "Bengaluru", URL: "https://x/1"}}
	if res, err := s.RecordCrawl(ctx, c.ID, board); err != nil || res.Notified != 2 {
		t.Fatalf("queued %d, %v; want 2", res.Notified, err)
	}
	if err := s.DeactivateUser(ctx, 200); err != nil { // priya pauses alerts
		t.Fatal(err)
	}

	claim := func() []Notification {
		t.Helper()
		ns, err := s.ClaimNotifications(ctx, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return ns
	}

	ns := claim()
	if len(ns) != 1 || ns[0].ChatID != 100 || ns[0].Company != "Razorpay" || ns[0].URL != "https://x/1" {
		t.Fatalf("claimed %+v, want only ravi's Razorpay alert", ns)
	}
	if again := claim(); len(again) != 0 {
		t.Errorf("leased notification was handed out again: %+v", again)
	}

	// A failure makes it due again after the delay; the last allowed attempt gives up.
	id := []int64{ns[0].ID}
	if err := s.RetryNotifications(ctx, id, 0, 2, "telegram 502"); err != nil {
		t.Fatal(err)
	}
	if ns := claim(); len(ns) != 1 || ns[0].Attempts != 1 {
		t.Fatalf("after one failure claimed %+v, want it back with 1 attempt", ns)
	}
	if err := s.RetryNotifications(ctx, id, 0, 2, "telegram 502"); err != nil {
		t.Fatal(err)
	}
	if ns := claim(); len(ns) != 0 {
		t.Errorf("claimed %+v after max attempts, want it given up", ns)
	}
	var status string
	s.pool.QueryRow(ctx, `SELECT status FROM notifications WHERE id = $1`, id[0]).Scan(&status)
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}
