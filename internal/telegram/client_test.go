package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "123456789:AAH-abcdefghijklmnopqrstuvwxyz_0123"

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.Client(), token, 42, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.BaseURL = srv.URL
	return c
}

func TestSendRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+token+"/sendMessage" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["chat_id"] != float64(42) || body["parse_mode"] != "HTML" || body["text"] != "<b>hi</b>" {
			t.Errorf("unexpected body %v", body)
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
	if err := c.Send(context.Background(), "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
}

func TestSend429UsesRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var first time.Time
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			first = time.Now()
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`)
		default:
			if d := time.Since(first); d < 900*time.Millisecond {
				t.Errorf("retried after %s, before retry_after", d)
			}
			io.WriteString(w, `{"ok":true}`)
		}
	})
	if err := c.Send(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSendRetries5xx(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	})
	if err := c.Send(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSend400IsPermanent(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
	})
	err := c.Send(context.Background(), "x")
	if !errors.Is(err, ErrRejected) || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestSendStopsOnContext(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"ok":false,"parameters":{"retry_after":30}}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Send(ctx, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not stop on context")
	}
}

func TestErrorsDoNotLeakToken(t *testing.T) {
	c := New(http.DefaultClient, token, 42, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.BaseURL = "http://127.0.0.1:1" // connection refused
	_, err := c.sendOnce(context.Background(), []byte("{}"))
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "AAH-") {
		t.Fatalf("err = %v", err)
	}
}
