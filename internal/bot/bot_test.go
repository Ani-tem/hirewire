package bot

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/match"
	"github.com/Ani-tem/hirewire/internal/store"
)

func TestParseFilter(t *testing.T) {
	tests := []struct {
		in      string
		want    match.Filter
		wantErr bool
	}{
		{"backend", match.Filter{Include: []string{"backend"}}, false},
		{"backend, sde, -senior, -staff, @bengaluru, @remote india", match.Filter{
			Include:   []string{"backend", "sde"},
			Exclude:   []string{"senior", "staff"},
			Locations: []string{"bengaluru", "remote india"},
		}, false},
		{"  machine learning ,, - lead , @ ", match.Filter{
			Include: []string{"machine learning"},
			Exclude: []string{"lead"},
		}, false},
		{"c++, c#", match.Filter{Include: []string{"c++", "c#"}}, false},
		{"", match.Filter{}, true},
		{"-senior, @bengaluru", match.Filter{}, true}, // would match every junior job everywhere in Bengaluru
	}
	for _, tt := range tests {
		got, err := parseFilter(tt.in)
		if (err != nil) != tt.wantErr || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseFilter(%q) = %+v, %v; want %+v, error=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestFormatFilterRoundTrips(t *testing.T) {
	in := "backend, sde, -senior, @bengaluru, @remote india"
	f, err := parseFilter(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatFilter(f); got != in {
		t.Errorf("formatFilter = %q, want %q", got, in)
	}
}

// fakeStore keeps users and subscriptions in maps.
type fakeStore struct {
	users  map[int64]bool // chat ID -> active
	subs   []store.Subscription
	jobs   []store.StoredJob
	nextID int64
}

func (f *fakeStore) AddUser(_ context.Context, chat int64) (int64, error) {
	f.users[chat] = true
	return chat, nil // user ID = chat ID keeps the fake simple
}

func (f *fakeStore) DeactivateUser(_ context.Context, chat int64) error {
	f.users[chat] = false
	return nil
}

func (f *fakeStore) AddSubscription(_ context.Context, user int64, m match.Filter) (int64, error) {
	f.nextID++
	f.subs = append(f.subs, store.Subscription{ID: f.nextID, UserID: user,
		IncludeKeywords: m.Include, ExcludeKeywords: m.Exclude, Locations: m.Locations})
	return f.nextID, nil
}

func (f *fakeStore) ListSubscriptions(_ context.Context, user int64) ([]store.Subscription, error) {
	var out []store.Subscription
	for _, s := range f.subs {
		if s.UserID == user {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteSubscription(_ context.Context, user, id int64) (bool, error) {
	n := len(f.subs)
	f.subs = slices.DeleteFunc(f.subs, func(s store.Subscription) bool { return s.ID == id && s.UserID == user })
	return len(f.subs) < n, nil
}

func (f *fakeStore) ListJobs(context.Context, time.Time) ([]store.StoredJob, error) {
	return f.jobs, nil
}
func (f *fakeStore) CountCompanies(context.Context) (int, error) { return 185, nil }

func newBot() (*Bot, *fakeStore) {
	fs := &fakeStore{users: map[int64]bool{}, jobs: []store.StoredJob{
		{Company: "FamPay", Title: "SDE 2 Infra", Location: "Bengaluru", URL: "https://x/1"},
		{Company: "GitLab", Title: "Intermediate Backend Engineer", Location: "Bangalore, India", URL: "https://x/2"},
		{Company: "Stripe", Title: "Senior Backend Engineer", Location: "Bengaluru", URL: "https://x/3"},
		{Company: "Figma", Title: "Product Designer", Location: "San Francisco", URL: "https://x/4"},
	}}
	return &Bot{Store: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fs
}

func TestConversation(t *testing.T) {
	b, fs := newBot()
	ctx := context.Background()
	const ravi, priya = 1, 2

	// Each step: who says what, and pieces of the reply we expect.
	steps := []struct {
		chat int64
		say  string
		want []string
	}{
		{ravi, "/start", []string{"185 tech companies", "<code>/subscribe backend, sde, -senior, @bengaluru</code>"}},
		{ravi, "hi there", []string{"I only understand commands", "/help"}},
		{ravi, "/list", []string{"no alerts yet"}},
		{ravi, "/subscribe", []string{"Tell me what to look for"}},
		{ravi, "/subscribe -senior, @bengaluru", []string{"at least one word the job title must contain"}},
		{ravi, "/subscribe backend, sde, -senior, @bengaluru", []string{
			"Alert #1 saved",
			"Title has: <b>backend</b> or <b>sde</b>",
			"But not: <b>senior</b>",
			"Location: <b>bengaluru</b>",
			"<b>2 jobs</b> matched", // FamPay and GitLab; Stripe's is senior, Figma's is design
			`<a href="https://x/1">SDE 2 Infra</a> at FamPay`,
		}},
		{ravi, "/subscribe@AniHire_bot quantum chef", []string{"Alert #2 saved", "Location: anywhere", "No jobs matched"}},
		{ravi, "/list", []string{"<b>#1</b>", "Remove: /unsubscribe 1", "<code>/subscribe backend, sde, -senior, @bengaluru</code>"}},
		{priya, "/unsubscribe 1", []string{"You don't have an alert #1"}}, // not hers
		{ravi, "/unsubscribe one", []string{"Which alert?"}},
		{ravi, "/unsubscribe #1", []string{"Alert #1 removed"}},
		{ravi, "/pause", []string{"Alerts paused"}},
		{ravi, "/resume", []string{"Alerts are on"}},
		{ravi, "/frobnicate", []string{"I don't know <code>/frobnicate</code>", "How to set up an alert"}},
	}
	for _, s := range steps {
		got := b.handle(ctx, s.chat, s.say)
		for _, w := range s.want {
			if !strings.Contains(got, w) {
				t.Errorf("%d says %q: reply missing %q\n--- reply ---\n%s", s.chat, s.say, w, got)
			}
		}
	}
	if !fs.users[ravi] {
		t.Error("ravi sent /resume but is still inactive")
	}
}

// Whatever a user types is echoed back inside Telegram HTML; unescaped, a "<"
// would make Telegram reject the reply, and a crafted tag could fake a link.
func TestUserTextIsEscaped(t *testing.T) {
	b, _ := newBot()
	tests := []struct{ say, want, never string }{
		{"/subscribe <u>go</u> & rust, @<i>pune</i>", "&lt;u&gt;go&lt;/u&gt; &amp; rust", "<u>"},
		{"/subscribe <u>go</u> & rust, @<i>pune</i>", "&lt;i&gt;pune&lt;/i&gt;", "<i>pune"},
		{"/<script>", "&lt;script&gt;", "<script>"},
	}
	for _, tt := range tests {
		got := b.handle(context.Background(), 1, tt.say)
		if !strings.Contains(got, tt.want) || strings.Contains(got, tt.never) {
			t.Errorf("%q: want %q and never %q in reply:\n%s", tt.say, tt.want, tt.never, got)
		}
	}
}

func TestPreviewWarnsWhenTooBroad(t *testing.T) {
	var jobs []store.StoredJob
	for range tooMany + 1 {
		jobs = append(jobs, store.StoredJob{Company: "Acme", Title: "Engineer", URL: "https://x"})
	}
	if got := preview(match.Filter{Include: []string{"engineer"}}, jobs); !strings.Contains(got, "That's a lot") {
		t.Errorf("no narrowing hint for %d matches:\n%s", len(jobs), got)
	}
}

func TestSubscriptionLimit(t *testing.T) {
	b, _ := newBot()
	for range maxSubscriptions {
		b.handle(context.Background(), 1, "/subscribe backend")
	}
	if got := b.handle(context.Background(), 1, "/subscribe backend"); !strings.Contains(got, "the most allowed") {
		t.Errorf("subscription %d: got %q, want a refusal", maxSubscriptions+1, got)
	}
}
