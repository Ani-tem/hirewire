package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAPI answers one bot method with body and records the JSON it was sent.
func fakeAPI(t *testing.T, method, body string, sent *map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botSECRET/"+method {
			http.NotFound(w, r)
			return
		}
		if sent != nil {
			json.NewDecoder(r.Body).Decode(sent)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	baseURL = srv.URL
}

func TestUpdates(t *testing.T) {
	var sent map[string]any
	fakeAPI(t, "getUpdates", `{"ok":true,"result":[
		{"update_id":7,"message":{"chat":{"id":42},"text":"/start"}},
		{"update_id":8,"edited_message":{"chat":{"id":42},"text":"edited"}}]}`, &sent)

	ups, err := New("SECRET", http.DefaultClient).Updates(context.Background(), 7, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].ID != 7 || ups[0].Message.Chat.ID != 42 || ups[0].Message.Text != "/start" {
		t.Errorf("bad first update: %+v", ups)
	}
	if ups[1].Message != nil {
		t.Errorf("an edit is not a message, got %+v", ups[1].Message)
	}
	if sent["offset"] != 7.0 || sent["timeout"] != 30.0 {
		t.Errorf("sent %v, want offset 7 and timeout 30", sent)
	}
}

func TestSendRateLimited(t *testing.T) {
	fakeAPI(t, "sendMessage", `{"ok":false,"error_code":429,
		"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`, nil)

	err := New("SECRET", http.DefaultClient).Send(context.Background(), 42, "hi")
	var tgErr *Error
	if !errors.As(err, &tgErr) {
		t.Fatalf("got %v, want a *telegram.Error", err)
	}
	if tgErr.Code != 429 || tgErr.RetryAfter != 3*time.Second {
		t.Errorf("got code %d, retry after %v; want 429 and 3s", tgErr.Code, tgErr.RetryAfter)
	}
}

func TestErrorsHideTheToken(t *testing.T) {
	baseURL = "http://127.0.0.1:1" // nothing listens here, so the request fails inside net/http
	err := New("SECRET", http.DefaultClient).Send(context.Background(), 42, "hi")
	if err == nil {
		t.Fatal("want a connection error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("token leaked into error: %v", err)
	}
}
