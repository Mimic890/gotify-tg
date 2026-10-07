package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mimic890/gotify-tg/internal/config"
	"github.com/mimic890/gotify-tg/internal/gotify"
	"github.com/mimic890/gotify-tg/internal/state"
)

func prio(p int) *int { return &p }

func testBridge(t *testing.T, queue int) *bridge {
	return &bridge{
		cfg:   &config.Config{MinPriority: 1, DataDir: t.TempDir()},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		queue: make(chan gotify.Message, queue),
	}
}

func TestOfferDedupesAndFilters(t *testing.T) {
	b := testBridge(t, 10)
	b.lastQueued = 5
	ctx := context.Background()
	for _, m := range []gotify.Message{
		{ID: 4, Priority: prio(5)}, // already forwarded
		{ID: 5, Priority: prio(5)}, // already forwarded
		{ID: 6, Priority: prio(5)},
		{ID: 6, Priority: prio(5)}, // duplicate from stream after catch-up
		{ID: 7, Priority: prio(0)}, // below MIN_PRIORITY
		{ID: 8},                    // no priority -> 0, filtered
		{ID: 9, Priority: prio(1)},
	} {
		if err := b.offer(ctx, m, false); err != nil {
			t.Fatal(err)
		}
	}
	close(b.queue)
	var got []uint
	for m := range b.queue {
		got = append(got, m.ID)
	}
	if !slices.Equal(got, []uint{6, 9}) || b.lastQueued != 9 {
		t.Fatalf("queued %v, lastQueued %d", got, b.lastQueued)
	}
}

func TestOfferQueueFullDoesNotAdvance(t *testing.T) {
	b := testBridge(t, 1)
	ctx := context.Background()
	if err := b.offer(ctx, gotify.Message{ID: 1, Priority: prio(1)}, false); err != nil {
		t.Fatal(err)
	}
	if err := b.offer(ctx, gotify.Message{ID: 2, Priority: prio(1)}, false); err != errQueueFull {
		t.Fatalf("err = %v", err)
	}
	if b.lastQueued != 1 {
		t.Fatalf("lastQueued = %d; the dropped message must be fetched again by catch-up", b.lastQueued)
	}
}

// TestBridgeEndToEnd runs the whole bridge against fake Gotify and
// Telegram servers: catch-up after restart, duplicates on the stream,
// filtering and state persistence.
func TestBridgeEndToEnd(t *testing.T) {
	msg := func(id uint, p int) gotify.Message {
		return gotify.Message{ID: id, AppID: 1, Title: "T", Message: fmt.Sprintf("body-%d", id), Priority: prio(p)}
	}
	stored := []gotify.Message{msg(1, 5), msg(2, 5), msg(3, 5), msg(4, 0), msg(5, 5)}
	live := []gotify.Message{msg(5, 5), msg(6, 5), msg(6, 5), msg(7, 0), msg(8, 5)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /message", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var page []gotify.Message
		for i := len(stored) - 1; i >= 0 && len(page) < limit; i-- {
			page = append(page, stored[i])
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": page, "paging": map[string]any{}})
	})
	mux.HandleFunc("GET /application", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":1,"name":"<Backup>"}]`)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for _, m := range live {
			b, _ := json.Marshal(m)
			c.Write(r.Context(), websocket.MessageText, b)
		}
		<-r.Context().Done()
	})
	gsrv := httptest.NewServer(mux)
	defer gsrv.Close()

	var mu sync.Mutex
	var sent []string
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = append(sent, body.Text)
		mu.Unlock()
		io.WriteString(w, `{"ok":true}`)
	}))
	defer tsrv.Close()

	dir := t.TempDir()
	if err := state.Save(dir, 2); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(gsrv.URL)
	cfg := &config.Config{GotifyURL: base, GotifyToken: "C", TelegramToken: "1:x", ChatID: 1, MinPriority: 1, DataDir: dir}
	b := newBridge(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.tg.BaseURL = tsrv.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout, sent %q", sent)
		case <-time.After(20 * time.Millisecond):
		}
	}
	time.Sleep(200 * time.Millisecond) // catch any late duplicates
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	want := []string{"body-3", "body-5", "body-6", "body-8"}
	if len(sent) != len(want) {
		t.Fatalf("sent %q", sent)
	}
	for i, w := range want {
		if !strings.HasSuffix(sent[i], w) || !strings.Contains(sent[i], "<i>&lt;Backup&gt;</i>") {
			t.Errorf("message %d = %q, want body %q", i, sent[i], w)
		}
	}
	if id, _, _ := state.Load(dir); id != 8 {
		t.Errorf("saved last_id = %d, want 8", id)
	}
	if err := state.CheckHealth(dir, time.Now(), time.Minute); err != nil {
		t.Errorf("health: %v", err)
	}
}
