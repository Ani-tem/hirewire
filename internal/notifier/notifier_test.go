package notifier

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Ani-tem/hirewire/internal/store"
	"github.com/Ani-tem/hirewire/internal/telegram"
)

// fakeStore records what the notifier decided for each notification.
type fakeStore struct {
	sent        []int64
	failed      []int64
	retried     map[int64]time.Duration // id -> delay
	deactivated []int64
}

func (f *fakeStore) ClaimNotifications(context.Context, int, time.Duration) ([]store.Notification, error) {
	return nil, nil
}
func (f *fakeStore) MarkSent(_ context.Context, ids []int64) error {
	f.sent = append(f.sent, ids...)
	return nil
}
func (f *fakeStore) RetryNotifications(_ context.Context, ids []int64, d time.Duration, _ int, _ string) error {
	for _, id := range ids {
		f.retried[id] = d
	}
	return nil
}
func (f *fakeStore) FailNotifications(_ context.Context, ids []int64, _ string) error {
	f.failed = append(f.failed, ids...)
	return nil
}
func (f *fakeStore) DeactivateUser(_ context.Context, chat int64) error {
	f.deactivated = append(f.deactivated, chat)
	return nil
}

// fakeTG answers each chat with a fixed error (nil = delivered) and keeps the texts it was sent.
type fakeTG struct {
	errs  map[int64]error
	texts map[int64][]string
}

func (f *fakeTG) Send(_ context.Context, chat int64, text string, _ ...telegram.Button) error {
	f.texts[chat] = append(f.texts[chat], text)
	return f.errs[chat]
}

func TestDeliver(t *testing.T) {
	const ok, blocked, flaky, limited = 1, 2, 3, 4 // messages go out in chat order; the 429 comes last so the test doesn't sit out its pause
	fs := &fakeStore{retried: map[int64]time.Duration{}}
	tg := &fakeTG{texts: map[int64][]string{}, errs: map[int64]error{
		blocked: &telegram.Error{Code: 403, Description: "Forbidden: bot was blocked by the user"},
		limited: &telegram.Error{Code: 429, RetryAfter: 7 * time.Second},
		flaky:   errors.New("connection reset"),
	}}
	n := &Notifier{Store: fs, TG: tg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	job := func(id, chat int64, attempts int) store.Notification {
		return store.Notification{ID: id, ChatID: chat, Attempts: attempts,
			Company: "Razorpay", Title: "Backend Engineer", Location: "Bengaluru", URL: "https://x"}
	}
	n.deliver(context.Background(), []store.Notification{
		job(10, ok, 0), job(20, blocked, 0), job(11, ok, 0), job(30, limited, 0), job(40, flaky, 2),
	})
	// The 429 pauses every send, not just that chat's.
	if d := time.Until(n.next); d < 6*time.Second {
		t.Errorf("after a 429 the next send may start in %v, want about 7s", d)
	}

	if len(tg.texts[ok]) != 1 || !strings.Contains(tg.texts[ok][0], "2 new jobs") {
		t.Errorf("chat %d got %q, want one message with both jobs", ok, tg.texts[ok])
	}
	if !equal(fs.sent, 10, 11) {
		t.Errorf("sent %v, want [10 11]", fs.sent)
	}
	if !equal(fs.failed, 20) || !equal(fs.deactivated, blocked) {
		t.Errorf("blocked: failed %v, deactivated %v; want [20] and [%d]", fs.failed, fs.deactivated, blocked)
	}
	if fs.retried[30] != 7*time.Second {
		t.Errorf("rate limited: retry in %v, want Telegram's 7s", fs.retried[30])
	}
	if fs.retried[40] != 2*time.Minute {
		t.Errorf("third failure: retry in %v, want 30s << 2 = 2m", fs.retried[40])
	}
}

func TestGroupSplitsLongBatches(t *testing.T) {
	var ns []store.Notification
	for i := range 23 {
		ns = append(ns, store.Notification{ID: int64(i), ChatID: 1})
	}
	ns = append(ns, store.Notification{ID: 99, ChatID: 2})

	ms := group(ns)
	var sizes []int64
	for _, m := range ms {
		sizes = append(sizes, int64(len(m.items)))
	}
	if !equal(sizes, 10, 10, 3, 1) {
		t.Errorf("message sizes %v, want [10 10 3 1]", sizes)
	}
}

func TestWaitRespectsTheGap(t *testing.T) {
	n := &Notifier{Gap: 50 * time.Millisecond}
	start := time.Now()
	for range 3 {
		n.wait(context.Background())
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("3 sends took %v, want at least 2 gaps (100ms)", took)
	}
}

func equal(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFormatEscapesAndAddsButtons(t *testing.T) {
	text, buttons := format([]store.Notification{
		{Company: "AT&T", Title: "Engineer <Platform>", Location: "Bengaluru", URL: "https://x/1"},
		{Company: "Acme", Title: "SDE", URL: "not a url"},
	})
	if strings.Contains(text, "<Platform>") || !strings.Contains(text, "Engineer &lt;Platform&gt;") || !strings.Contains(text, "AT&amp;T") {
		t.Errorf("job text not escaped:\n%s", text)
	}
	if len(buttons) != 1 || buttons[0].URL != "https://x/1" || buttons[0].Text != "1. Apply at AT&T" {
		t.Errorf("buttons = %+v, want one Apply button for the job with a real URL", buttons)
	}
	if !strings.Contains(text, "not a url") {
		t.Errorf("job without a usable URL should show it as text:\n%s", text)
	}
}
