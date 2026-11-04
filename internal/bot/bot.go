// Package bot is the Telegram side of hirewire: users manage their subscriptions
// by chatting with it.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Ani-tem/hirewire/internal/match"
	"github.com/Ani-tem/hirewire/internal/store"
	"github.com/Ani-tem/hirewire/internal/telegram"
)

const (
	pollWait         = 30 * time.Second // how long Telegram may hold a getUpdates open
	retryDelay       = 5 * time.Second  // pause after a failed poll, so an outage isn't a hot loop
	maxSubscriptions = 10               // per user, so one chat can't make every crawl match thousands of filters
)

const help = `I message you the moment a matching job is posted on one of the company boards hirewire watches.

/subscribe backend, sde, -senior, @bengaluru, @remote india
    word   must be in the job title (any one of them)
    -word  must not be in the title
    @place where the job is (any one of them)
/list  your subscriptions
/unsubscribe 3  remove subscription 3
/stop  pause all alerts; /start resumes them`

// Store is what the bot needs from the database. *store.Store satisfies it.
type Store interface {
	AddUser(ctx context.Context, telegramChatID int64) (int64, error)
	DeactivateUser(ctx context.Context, telegramChatID int64) error
	AddSubscription(ctx context.Context, userID int64, f match.Filter) (int64, error)
	ListSubscriptions(ctx context.Context, userID int64) ([]store.Subscription, error)
	DeleteSubscription(ctx context.Context, userID, subscriptionID int64) (bool, error)
}

type Bot struct {
	Store Store
	TG    *telegram.Client
	Log   *slog.Logger
}

// Run answers messages until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	var offset int64
	for {
		ups, err := b.TG.Updates(ctx, offset, pollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.Log.Warn("telegram poll failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			continue
		}
		for _, u := range ups {
			offset = u.ID + 1 // confirmed on the next poll; a crash before that replays these
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			chat := u.Message.Chat.ID
			reply := b.handle(ctx, chat, u.Message.Text)
			if err := b.TG.Send(ctx, chat, reply); err != nil {
				b.Log.Warn("reply failed", "chat", chat, "err", err)
			}
		}
	}
}

// handle turns one message into the reply text. It never talks to Telegram, so tests can call it directly.
func (b *Bot) handle(ctx context.Context, chatID int64, text string) string {
	cmd, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	cmd, _, _ = strings.Cut(cmd, "@") // in groups commands arrive as /list@hirewire_bot

	if cmd == "/stop" {
		if err := b.Store.DeactivateUser(ctx, chatID); err != nil {
			return b.fail(err)
		}
		return "Alerts paused. Your subscriptions are saved; send /start to resume."
	}

	// Any other message means the user is here: register them, or wake them up after /stop.
	userID, err := b.Store.AddUser(ctx, chatID)
	if err != nil {
		return b.fail(err)
	}

	switch cmd {
	case "/start", "/help":
		return help
	case "/subscribe":
		f, err := parseFilter(args)
		if err != nil {
			return err.Error() + "\n\nFor example:\n/subscribe backend, sde, -senior, @bengaluru"
		}
		subs, err := b.Store.ListSubscriptions(ctx, userID)
		if err != nil {
			return b.fail(err)
		}
		if len(subs) >= maxSubscriptions {
			return fmt.Sprintf("You already have %d subscriptions, the most allowed. Remove one with /unsubscribe first.", len(subs))
		}
		id, err := b.Store.AddSubscription(ctx, userID, f)
		if err != nil {
			return b.fail(err)
		}
		return fmt.Sprintf("Subscribed (%d): %s\nYou'll hear about new matching jobs from now on.", id, formatFilter(f))
	case "/list":
		subs, err := b.Store.ListSubscriptions(ctx, userID)
		if err != nil {
			return b.fail(err)
		}
		if len(subs) == 0 {
			return "No subscriptions yet. Add one:\n/subscribe backend, sde, -senior, @bengaluru"
		}
		lines := make([]string, len(subs))
		for i, s := range subs {
			lines[i] = fmt.Sprintf("%d: %s", s.ID, formatFilter(match.Filter{
				Include: s.IncludeKeywords, Exclude: s.ExcludeKeywords, Locations: s.Locations}))
		}
		return strings.Join(lines, "\n")
	case "/unsubscribe":
		id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
		if err != nil {
			return "Which one? Send /unsubscribe followed by a number from /list."
		}
		ok, err := b.Store.DeleteSubscription(ctx, userID, id)
		if err != nil {
			return b.fail(err)
		}
		if !ok {
			return fmt.Sprintf("You have no subscription %d. /list shows yours.", id)
		}
		return fmt.Sprintf("Removed subscription %d.", id)
	default:
		return "I don't know that one.\n\n" + help
	}
}

// fail logs the real error and gives the user a reply that leaks nothing.
func (b *Bot) fail(err error) string {
	b.Log.Error("bot command failed", "err", err)
	return "Something went wrong on our side. Please try again in a minute."
}

// parseFilter reads "backend, sde, -senior, @bengaluru": plain items are title
// keywords, "-" items are excluded keywords and "@" items are locations.
func parseFilter(s string) (match.Filter, error) {
	var f match.Filter
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		switch {
		case strings.HasPrefix(item, "-"):
			if k := strings.TrimSpace(item[1:]); k != "" {
				f.Exclude = append(f.Exclude, k)
			}
		case strings.HasPrefix(item, "@"):
			if k := strings.TrimSpace(item[1:]); k != "" {
				f.Locations = append(f.Locations, k)
			}
		case item != "":
			f.Include = append(f.Include, item)
		}
	}
	// No include keyword would match every job on every board: that's spam, not an alert.
	if len(f.Include) == 0 {
		return match.Filter{}, errors.New("Add at least one word the job title must contain.")
	}
	return f, nil
}

// formatFilter is the inverse of parseFilter, so /list output can be pasted back into /subscribe.
func formatFilter(f match.Filter) string {
	var items []string
	items = append(items, f.Include...)
	for _, k := range f.Exclude {
		items = append(items, "-"+k)
	}
	for _, k := range f.Locations {
		items = append(items, "@"+k)
	}
	return strings.Join(items, ", ")
}
