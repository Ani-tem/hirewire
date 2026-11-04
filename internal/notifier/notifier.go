// Package notifier delivers the pending notifications the crawler queued in the outbox.
package notifier

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Ani-tem/hirewire/internal/store"
	"github.com/Ani-tem/hirewire/internal/telegram"
)

const (
	tick        = 2 * time.Second  // how often we look for pending notifications when idle
	lease       = 2 * time.Minute  // a claimed notification stays ours this long, even if we crash
	batch       = 100              // notifications claimed per round
	perMessage  = 10               // jobs per Telegram message; keeps us far below the 4096-char limit
	maxAttempts = 6                // then the notification is marked failed
	baseBackoff = 30 * time.Second // 30s, 1m, 2m, 4m, 8m between attempts
	sendGap     = 40 * time.Millisecond
)

// Store is what the notifier needs from the database. *store.Store satisfies it.
type Store interface {
	ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]store.Notification, error)
	MarkSent(ctx context.Context, ids []int64) error
	RetryNotifications(ctx context.Context, ids []int64, delay time.Duration, maxAttempts int, reason string) error
	FailNotifications(ctx context.Context, ids []int64, reason string) error
	DeactivateUser(ctx context.Context, telegramChatID int64) error
}

// Sender sends one message to a chat. *telegram.Client satisfies it.
type Sender interface {
	Send(ctx context.Context, chatID int64, text string) error
}

type Notifier struct {
	Store Store
	TG    Sender
	Log   *slog.Logger
	// Gap is the minimum time between two sends. Telegram allows about 30
	// messages a second across all chats; 40ms keeps us at 25.
	Gap time.Duration

	next time.Time // earliest moment the next send may start
}

func New(s Store, tg Sender, log *slog.Logger) *Notifier {
	return &Notifier{Store: s, TG: tg, Log: log, Gap: sendGap}
}

// Run delivers notifications until ctx is cancelled.
// shortcut: polls every 2s; Postgres LISTEN/NOTIFY from RecordCrawl would wake it instantly.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		ns, err := n.Store.ClaimNotifications(ctx, batch, lease)
		if err != nil && ctx.Err() == nil {
			n.Log.Error("claim notifications", "err", err)
		}
		n.deliver(ctx, ns)
		if len(ns) == batch {
			continue // there may be more waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// message is one Telegram message: a chat and the notifications it carries.
type message struct {
	chat  int64
	items []store.Notification
}

// deliver sends ns, one message per chat (split every perMessage jobs), and records each outcome.
func (n *Notifier) deliver(ctx context.Context, ns []store.Notification) {
	for _, m := range group(ns) {
		if !n.wait(ctx) {
			return // shutting down: unsent notifications come back when their lease runs out
		}
		ids := make([]int64, len(m.items))
		for i, it := range m.items {
			ids[i] = it.ID
		}

		err := n.TG.Send(ctx, m.chat, format(m.items))
		if err != nil && ctx.Err() != nil {
			return // interrupted, not Telegram's fault: don't count an attempt
		}
		if err := n.record(ctx, m, ids, err); err != nil {
			n.Log.Error("record delivery", "chat", m.chat, "err", err)
		}
	}
}

// record saves the outcome of one send. The policy lives here; the store just persists it.
func (n *Notifier) record(ctx context.Context, m message, ids []int64, sendErr error) error {
	if sendErr == nil {
		return n.Store.MarkSent(ctx, ids)
	}
	n.Log.Warn("send failed", "chat", m.chat, "jobs", len(ids), "err", sendErr)

	var tgErr *telegram.Error
	if errors.As(sendErr, &tgErr) {
		switch {
		case tgErr.Code == 429:
			// Rate limited: Telegram says exactly how long to wait. Pause every send, not just this chat's.
			n.next = time.Now().Add(tgErr.RetryAfter)
			return n.Store.RetryNotifications(ctx, ids, tgErr.RetryAfter, maxAttempts, sendErr.Error())
		case tgErr.Code == 403:
			// The user blocked the bot. Retrying can't help, and they shouldn't get anything else.
			if err := n.Store.DeactivateUser(ctx, m.chat); err != nil {
				return err
			}
			return n.Store.FailNotifications(ctx, ids, sendErr.Error())
		case tgErr.Code == 400:
			return n.Store.FailNotifications(ctx, ids, sendErr.Error()) // the request itself is wrong; it will be wrong again
		}
	}
	// Anything else (5xx, timeout, network) is probably temporary: back off and try again.
	return n.Store.RetryNotifications(ctx, ids, backoff(m.items), maxAttempts, sendErr.Error())
}

// wait blocks until the rate limit allows another send. It reports false if ctx ended first.
func (n *Notifier) wait(ctx context.Context) bool {
	if d := time.Until(n.next); d > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
		}
	}
	n.next = time.Now().Add(n.Gap)
	return ctx.Err() == nil
}

// backoff is the delay before the next attempt: baseBackoff doubled for each attempt already made.
func backoff(items []store.Notification) time.Duration {
	attempts := 0
	for _, it := range items {
		attempts = max(attempts, it.Attempts)
	}
	return baseBackoff << attempts
}

// group puts each chat's notifications into messages of at most perMessage jobs, in claim order.
func group(ns []store.Notification) []message {
	ns = slices.Clone(ns)
	slices.SortStableFunc(ns, func(a, b store.Notification) int { return cmp.Compare(a.ChatID, b.ChatID) })
	var ms []message
	for _, it := range ns {
		if len(ms) == 0 || ms[len(ms)-1].chat != it.ChatID || len(ms[len(ms)-1].items) == perMessage {
			ms = append(ms, message{chat: it.ChatID})
		}
		ms[len(ms)-1].items = append(ms[len(ms)-1].items, it)
	}
	return ms
}

// format writes the message text. Plain text: job titles can contain characters
// that Telegram's Markdown or HTML modes would need escaped.
func format(items []store.Notification) string {
	var b strings.Builder
	if len(items) == 1 {
		b.WriteString("New job\n\n")
	} else {
		fmt.Fprintf(&b, "%d new jobs\n\n", len(items))
	}
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s: %s\n", it.Company, it.Title)
		if it.Location != "" {
			fmt.Fprintf(&b, "%s\n", it.Location)
		}
		b.WriteString(it.URL)
	}
	return b.String()
}
