// Package telegram is a minimal client for the two Bot API calls hirewire needs:
// receiving messages (getUpdates) and sending them (sendMessage).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

var baseURL = "https://api.telegram.org"

type Client struct {
	token string
	http  *http.Client
}

func New(token string, client *http.Client) *Client {
	return &Client{token: token, http: client}
}

// Update is one incoming event. Only text messages matter to us; other kinds have a nil Message.
type Update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// Error is a request Telegram refused. RetryAfter is set when we were rate limited (code 429).
type Error struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

// Updates long-polls for messages: Telegram holds the request open for up to wait
// until something arrives, so we get messages instantly without hammering the API.
// Passing offset = last ID + 1 confirms everything before it; unconfirmed updates
// are delivered again, e.g. after a restart.
func (c *Client) Updates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(wait.Seconds()),
		"allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

// Button opens URL when tapped. It is shown under the message.
type Button struct {
	Text string
	URL  string
}

// Send posts a message to a chat, one button per row under it. text is Telegram
// HTML (<b>, <i>, <code>, <a href>): anything from outside, like a job title,
// must go through html.EscapeString first, or Telegram rejects the whole message.
func (c *Client) Send(ctx context.Context, chatID int64, text string, buttons ...Button) error {
	params := map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if len(buttons) > 0 {
		rows := make([][]map[string]string, len(buttons))
		for i, b := range buttons {
			rows[i] = []map[string]string{{"text": b.Text, "url": b.URL}}
		}
		params["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	return c.call(ctx, "sendMessage", params, nil)
}

// Command is one entry in the menu Telegram shows when a user types "/".
type Command struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}

// SetProfile sets the bot's command menu, its description (shown in an empty
// chat, before the user has sent anything) and its short description (shown on its profile).
func (c *Client) SetProfile(ctx context.Context, commands []Command, description, short string) error {
	if err := c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); err != nil {
		return err
	}
	if err := c.call(ctx, "setMyDescription", map[string]any{"description": description}, nil); err != nil {
		return err
	}
	return c.call(ctx, "setMyShortDescription", map[string]any{"short_description": short}, nil)
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	// The token is part of the URL, so it must never reach a log line.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", baseURL, c.token, method), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method) // the error would quote the URL
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = "[redacted]" // net/http errors quote the full URL, token included
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	// Telegram answers every call, success or not, with this envelope.
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: status %d: %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return &Error{
			Code:        env.ErrorCode,
			Description: env.Description,
			RetryAfter:  time.Duration(env.Parameters.RetryAfter) * time.Second,
		}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, result); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}
