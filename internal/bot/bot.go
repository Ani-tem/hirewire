// Package bot is the Telegram side of hirewire: users manage their subscriptions
// by chatting with it.
//
// Replies are Telegram HTML. Anything not written here (keywords users type,
// job titles, company names) goes through esc, or one stray "<" or "&" makes
// Telegram reject the whole message.
package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
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
	previewWindow    = 30 * 24 * time.Hour
	previewExamples  = 3
	tooMany          = 50 // matches in previewWindow above which we suggest narrowing the filter
)

// The menu Telegram shows when a user types "/", and the text in an empty chat before /start.
var (
	commands = []telegram.Command{
		{Name: "subscribe", Description: "Get alerts for jobs you want"},
		{Name: "list", Description: "See your subscriptions"},
		{Name: "unsubscribe", Description: "Remove a subscription"},
		{Name: "pause", Description: "Stop all alerts for now"},
		{Name: "resume", Description: "Start alerts again"},
		{Name: "help", Description: "How it works"},
	}
	description = "I watch the careers pages of tech companies (Stripe, NVIDIA, OpenAI, CRED, Meesho, Razorpay and many more) " +
		"and message you when a job you want is posted, usually within 15 minutes.\n\nTap Start to set up your first alert."
	shortDescription = "Instant alerts for new tech jobs, straight from company careers pages."
)

// Store is what the bot needs from the database. *store.Store satisfies it.
type Store interface {
	AddUser(ctx context.Context, telegramChatID int64) (int64, error)
	DeactivateUser(ctx context.Context, telegramChatID int64) error
	AddSubscription(ctx context.Context, userID int64, f match.Filter) (int64, error)
	ListSubscriptions(ctx context.Context, userID int64) ([]store.Subscription, error)
	DeleteSubscription(ctx context.Context, userID, subscriptionID int64) (bool, error)
	ListJobs(ctx context.Context, since time.Time) ([]store.StoredJob, error)
	CountCompanies(ctx context.Context) (int, error)
}

type Bot struct {
	Store Store
	TG    *telegram.Client
	Log   *slog.Logger
}

// Run answers messages until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	if err := b.TG.SetProfile(ctx, commands, description, shortDescription); err != nil {
		b.Log.Warn("set bot profile", "err", err) // cosmetic: keep answering messages anyway
	}
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

// handle turns one message into the reply. It never talks to Telegram, so tests can call it directly.
func (b *Bot) handle(ctx context.Context, chatID int64, text string) string {
	cmd, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	cmd, _, _ = strings.Cut(cmd, "@") // in groups commands arrive as /list@AniHire_bot
	cmd = strings.ToLower(cmd)

	if cmd == "/pause" || cmd == "/stop" {
		if err := b.Store.DeactivateUser(ctx, chatID); err != nil {
			return b.fail(err)
		}
		return "⏸ <b>Alerts paused.</b>\n\nYour subscriptions are kept. Send /resume whenever you want alerts again."
	}

	// Any other message means the user is here: register them, or wake them up after /pause.
	userID, err := b.Store.AddUser(ctx, chatID)
	if err != nil {
		return b.fail(err)
	}

	switch cmd {
	case "/start":
		return b.welcome(ctx)
	case "/help":
		return help
	case "/resume":
		return "▶️ <b>Alerts are on.</b>\n\nYou'll get new jobs that match your subscriptions. /list shows them."
	case "/subscribe":
		return b.subscribe(ctx, userID, args)
	case "/list":
		return b.list(ctx, userID)
	case "/unsubscribe":
		return b.unsubscribe(ctx, userID, args)
	}
	if strings.HasPrefix(cmd, "/") {
		return "I don't know <code>" + esc(cmd) + "</code>.\n\n" + help
	}
	return "I only understand commands. To get job alerts, send something like:\n\n" + examples +
		"\n\n/help explains how it works."
}

// examples are ready-made subscriptions; <code> makes each one copy on tap in Telegram.
const examples = `<code>/subscribe backend, sde, -senior, @bengaluru</code>
<code>/subscribe frontend, react, @remote india</code>
<code>/subscribe new grad, graduate, intern, @india</code>`

const help = `<b>How to set up an alert</b>

Send /subscribe followed by what you want, separated by commas:

• <b>word</b> — the job title must contain at least one of these
• <b>-word</b> — skip jobs whose title contains this
• <b>@place</b> — the job must be in one of these places (leave out for anywhere)

For example:
<code>/subscribe backend, sde, -senior, -staff, @bengaluru, @remote india</code>
means: titles with "backend" or "sde", but not "senior" or "staff", in Bengaluru or remote India.

Matching ignores case and works on whole words, so "java" won't match "JavaScript". Common variants are understood: Bangalore = Bengaluru, Sr = Senior, SDE II = SDE 2.

<b>Commands</b>
/subscribe — add an alert (up to 10)
/list — see your alerts
/unsubscribe 2 — remove alert #2
/pause and /resume — stop or restart all alerts`

func (b *Bot) welcome(ctx context.Context) string {
	n, err := b.Store.CountCompanies(ctx)
	if err != nil {
		return b.fail(err)
	}
	return fmt.Sprintf(`👋 <b>Welcome to hirewire!</b>

I watch the careers pages of <b>%d tech companies</b> and message you as soon as a job you want is posted, usually within 15 minutes, often before it shows up on LinkedIn.

<b>Set up your first alert.</b> Tap one of these to copy it, edit it, and send:

%s

/help explains the format.`, n, examples)
}

func (b *Bot) subscribe(ctx context.Context, userID int64, args string) string {
	if strings.TrimSpace(args) == "" {
		return "Tell me what to look for after /subscribe. For example:\n\n" + examples + "\n\n/help explains the format."
	}
	f, err := parseFilter(args)
	if err != nil {
		return esc(err.Error()) + "\n\nFor example:\n<code>/subscribe backend, sde, -senior, @bengaluru</code>"
	}
	subs, err := b.Store.ListSubscriptions(ctx, userID)
	if err != nil {
		return b.fail(err)
	}
	if len(subs) >= maxSubscriptions {
		return fmt.Sprintf("You already have %d alerts, the most allowed. Remove one first: /list shows them with their numbers.", len(subs))
	}
	jobs, err := b.Store.ListJobs(ctx, time.Now().Add(-previewWindow))
	if err != nil {
		return b.fail(err)
	}
	id, err := b.Store.AddSubscription(ctx, userID, f)
	if err != nil {
		return b.fail(err)
	}

	var r strings.Builder
	fmt.Fprintf(&r, "✅ <b>Alert #%d saved.</b> You'll get jobs where:\n\n%s\n\n", id, describe(f))
	r.WriteString(preview(f, jobs))
	return r.String()
}

// preview shows how the filter does on recent jobs, so a user can tell right away
// whether it's too broad or too narrow instead of waiting days for silence.
func preview(f match.Filter, jobs []store.StoredJob) string {
	m := match.Compile(f)
	var hits []store.StoredJob
	for _, j := range jobs {
		if m.Match(j.Title, j.Location) {
			hits = append(hits, j)
		}
	}
	var r strings.Builder
	switch {
	case len(hits) == 0:
		r.WriteString("No jobs matched this in the last 30 days. That's fine for a specific search, " +
			"but if you expected some, try more title words or fewer exclusions.\n\n")
	default:
		fmt.Fprintf(&r, "<b>%d jobs</b> matched this in the last 30 days, for example:\n", len(hits))
		for _, j := range hits[:min(previewExamples, len(hits))] {
			fmt.Fprintf(&r, "• <a href=\"%s\">%s</a> at %s\n", esc(j.URL), esc(j.Title), esc(j.Company))
		}
		r.WriteString("\n")
		if len(hits) > tooMany {
			r.WriteString("That's a lot of messages. To narrow it, add <code>-word</code> to skip titles or <code>@place</code> for location.\n\n")
		}
	}
	r.WriteString("From now on I'll message you when <b>new</b> ones are posted. /list shows all your alerts.")
	return r.String()
}

func (b *Bot) list(ctx context.Context, userID int64) string {
	subs, err := b.Store.ListSubscriptions(ctx, userID)
	if err != nil {
		return b.fail(err)
	}
	if len(subs) == 0 {
		return "You have no alerts yet. Tap one of these to copy it, edit it, and send:\n\n" + examples
	}
	var r strings.Builder
	r.WriteString("<b>Your alerts</b>\n")
	for _, s := range subs {
		f := match.Filter{Include: s.IncludeKeywords, Exclude: s.ExcludeKeywords, Locations: s.Locations}
		fmt.Fprintf(&r, "\n<b>#%d</b>\n%s\nRemove: /unsubscribe %d\n", s.ID, describe(f), s.ID)
	}
	r.WriteString("\nTo change an alert, remove it and send it again. Tap to copy one:\n")
	for _, s := range subs {
		f := match.Filter{Include: s.IncludeKeywords, Exclude: s.ExcludeKeywords, Locations: s.Locations}
		fmt.Fprintf(&r, "<code>/subscribe %s</code>\n", esc(formatFilter(f)))
	}
	return r.String()
}

func (b *Bot) unsubscribe(ctx context.Context, userID int64, args string) string {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(args), "#"), 10, 64)
	if err != nil {
		return "Which alert? Send /unsubscribe with its number, like <code>/unsubscribe 2</code>. /list shows the numbers."
	}
	ok, err := b.Store.DeleteSubscription(ctx, userID, id)
	if err != nil {
		return b.fail(err)
	}
	if !ok {
		return fmt.Sprintf("You don't have an alert #%d. /list shows yours.", id)
	}
	return fmt.Sprintf("🗑 Alert #%d removed.", id)
}

// describe says in plain words what a filter matches.
func describe(f match.Filter) string {
	lines := []string{"• Title has: " + join(f.Include, " or ")}
	if len(f.Exclude) > 0 {
		lines = append(lines, "• But not: "+join(f.Exclude, ", "))
	}
	if len(f.Locations) > 0 {
		lines = append(lines, "• Location: "+join(f.Locations, " or "))
	} else {
		lines = append(lines, "• Location: anywhere")
	}
	return strings.Join(lines, "\n")
}

// join bolds and escapes each word, then joins them with sep.
func join(words []string, sep string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = "<b>" + esc(w) + "</b>"
	}
	return strings.Join(out, sep)
}

func esc(s string) string { return html.EscapeString(s) }

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
		return match.Filter{}, errors.New("Add at least one word the job title must contain, like backend. " +
			"Without one, every job in your locations would match.")
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
