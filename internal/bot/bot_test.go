package bot

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

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

func TestConversation(t *testing.T) {
	fs := &fakeStore{users: map[int64]bool{}}
	b := &Bot{Store: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	const ravi, priya = 1, 2

	// Each step: who says what, and a piece of the reply we expect.
	steps := []struct {
		chat      int64
		say, want string
	}{
		{ravi, "/start", "/subscribe backend"},
		{ravi, "/list", "No subscriptions yet"},
		{ravi, "/subscribe", "at least one word"},
		{ravi, "/subscribe backend, -senior, @bengaluru", "Subscribed (1): backend, -senior, @bengaluru"},
		{ravi, "/subscribe@hirewire_bot sde", "Subscribed (2): sde"},
		{ravi, "/list", "1: backend, -senior, @bengaluru\n2: sde"},
		{priya, "/unsubscribe 1", "You have no subscription 1"}, // not hers
		{ravi, "/unsubscribe one", "followed by a number"},
		{ravi, "/unsubscribe 1", "Removed subscription 1"},
		{ravi, "/list", "2: sde"},
		{ravi, "/stop", "Alerts paused"},
		{ravi, "hello", "I don't know that one"},
	}
	for _, s := range steps {
		if got := b.handle(ctx, s.chat, s.say); !strings.Contains(got, s.want) {
			t.Errorf("%d says %q: got %q, want it to contain %q", s.chat, s.say, got, s.want)
		}
	}
	// Any message after /stop counts as coming back.
	if !fs.users[ravi] {
		t.Error("ravi wrote after /stop but is still inactive")
	}
}

func TestSubscriptionLimit(t *testing.T) {
	fs := &fakeStore{users: map[int64]bool{}}
	b := &Bot{Store: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for range maxSubscriptions {
		b.handle(context.Background(), 1, "/subscribe backend")
	}
	if got := b.handle(context.Background(), 1, "/subscribe backend"); !strings.Contains(got, "the most allowed") {
		t.Errorf("subscription %d: got %q, want a refusal", maxSubscriptions+1, got)
	}
}
