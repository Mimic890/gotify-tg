package gotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeServer emulates Gotify's GET /message paging: newest first, "since"
// returns IDs strictly below it, and "next" is a relative URL.
func fakeServer(t *testing.T, ids []uint, stream func(*websocket.Conn)) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gotify/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gotify-Key") != "Ctoken" || r.URL.Query().Has("token") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		desc := slices.Clone(ids)
		slices.Sort(desc)
		slices.Reverse(desc)
		var page []Message
		for _, id := range desc {
			if since == 0 || id < uint(since) {
				page = append(page, Message{ID: id, AppID: 1, Message: fmt.Sprint("m", id)})
			}
		}
		next := ""
		if len(page) > limit {
			page = page[:limit]
			next = fmt.Sprintf("/message?limit=%d&since=%d", limit, page[len(page)-1].ID)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"messages": page,
			"paging":   map[string]any{"next": next, "limit": limit, "size": len(page)},
		})
	})
	mux.HandleFunc("GET /gotify/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gotify-Key") != "Ctoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		stream(c)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL + "/gotify")
	return New(base, "Ctoken", srv.Client(), &http.Client{})
}

func ids(msgs []Message) []uint {
	out := []uint{}
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

func TestMessagesAfterPagesAndOrders(t *testing.T) {
	var all []uint
	for i := uint(1); i <= 450; i++ {
		if i%7 != 0 { // gaps from deleted messages
			all = append(all, i)
		}
	}
	c := fakeServer(t, all, nil)
	got, err := c.MessagesAfter(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var want []uint
	for _, id := range all {
		if id > 20 {
			want = append(want, id)
		}
	}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("got %d messages, want %d (first %v)", len(got), len(want), ids(got)[:5])
	}
}

func TestMessagesAfterNothingNew(t *testing.T) {
	c := fakeServer(t, []uint{1, 2, 3}, nil)
	got, err := c.MessagesAfter(context.Background(), 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", ids(got), err)
	}
	latest, err := c.LatestID(context.Background())
	if err != nil || latest != 3 {
		t.Fatalf("latest = %d, %v", latest, err)
	}
}

func TestCollectAfterStopsWithoutProgress(t *testing.T) {
	calls := 0
	_, err := collectAfter(0, func(before uint) ([]Message, bool, error) {
		calls++
		return []Message{{ID: 5}, {ID: 5}}, true, nil // misbehaving server
	})
	if err != nil || calls > 2 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestCollectAfterDedupes(t *testing.T) {
	pages := map[uint][]Message{
		0: {{ID: 10}, {ID: 9}, {ID: 8}},
		8: {{ID: 8}, {ID: 7}, {ID: 6}}, // overlapping page
		6: {{ID: 5}, {ID: 4}},
	}
	got, err := collectAfter(4, func(before uint) ([]Message, bool, error) {
		return pages[before], true, nil
	})
	if err != nil || !slices.Equal(ids(got), []uint{5, 6, 7, 8, 9, 10}) {
		t.Fatalf("got %v, %v", ids(got), err)
	}
}

func TestStreamReadsMessages(t *testing.T) {
	c := fakeServer(t, nil, func(conn *websocket.Conn) {
		ctx := context.Background()
		for _, id := range []uint{11, 12} {
			b, _ := json.Marshal(Message{ID: id, AppID: 2, Title: "t", Message: "m"})
			conn.Write(ctx, websocket.MessageText, b)
		}
		conn.Close(websocket.StatusNormalClosure, "")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got []uint
	err = s.Read(ctx, func() {}, func(m Message) error {
		got = append(got, m.ID)
		return nil
	})
	if err == nil {
		t.Fatal("expected error after server closed")
	}
	if !slices.Equal(got, []uint{11, 12}) {
		t.Fatalf("got %v", got)
	}
}

func TestConnectUnauthorized(t *testing.T) {
	c := fakeServer(t, nil, nil)
	c.token = "wrong"
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
