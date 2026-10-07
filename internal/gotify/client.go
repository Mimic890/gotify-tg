// Package gotify reads messages from a Gotify server using a client token.
package gotify

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

const (
	pageSize        = 200 // maximum accepted by GET /message
	maxListSize     = 16 << 20
	maxStreamFrame  = 1 << 20
	pingInterval    = 30 * time.Second
	pingTimeout     = 10 * time.Second
	dialTimeout     = 20 * time.Second
	tokenHeaderName = "X-Gotify-Key"
)

// Message is a Gotify message as returned by /message and /stream.
type Message struct {
	ID       uint   `json:"id"`
	AppID    uint   `json:"appid"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority *int   `json:"priority"`
}

// Prio returns the priority, treating a missing value as 0.
func (m Message) Prio() int {
	if m.Priority == nil {
		return 0
	}
	return *m.Priority
}

// Client talks to one Gotify server. The token is always sent in the
// X-Gotify-Key header, never in the URL.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client // REST, with an overall timeout
	ws    *http.Client // WebSocket, no overall timeout (long-lived)
}

// New returns a client. rest must have a timeout; ws must not.
func New(base *url.URL, token string, rest, ws *http.Client) *Client {
	return &Client{base: base, token: token, http: rest, ws: ws}
}

func (c *Client) endpoint(path string, q url.Values) string {
	u := *c.base
	u.Path += path
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path, q), nil)
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeaderName, c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxListSize))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

type messagePage struct {
	Messages []Message `json:"messages"`
	Paging   struct {
		Next string `json:"next"`
	} `json:"paging"`
}

// page fetches up to limit messages with an ID below before (0 = newest),
// newest first.
func (c *Client) page(ctx context.Context, before uint, limit int) (messagePage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before > 0 {
		q.Set("since", strconv.FormatUint(uint64(before), 10))
	}
	var p messagePage
	err := c.getJSON(ctx, "/message", q, &p)
	return p, err
}

// LatestID returns the ID of the newest message, or 0 if there are none.
func (c *Client) LatestID(ctx context.Context) (uint, error) {
	p, err := c.page(ctx, 0, 1)
	if err != nil || len(p.Messages) == 0 {
		return 0, err
	}
	return p.Messages[0].ID, nil
}

// MessagesAfter returns all messages with an ID greater than after,
// oldest first. Gotify's "since" pages backwards (IDs below since), so
// this walks from the newest message down until it reaches after.
func (c *Client) MessagesAfter(ctx context.Context, after uint) ([]Message, error) {
	return collectAfter(after, func(before uint) ([]Message, bool, error) {
		p, err := c.page(ctx, before, pageSize)
		return p.Messages, p.Paging.Next != "", err
	})
}

// collectAfter pages backwards with fetch until it reaches IDs <= after
// and returns the newer messages in ascending ID order without duplicates.
func collectAfter(after uint, fetch func(before uint) (msgs []Message, more bool, err error)) ([]Message, error) {
	var out []Message
	seen := map[uint]bool{}
	var before uint
	for {
		msgs, more, err := fetch(before)
		if err != nil {
			return nil, err
		}
		done := !more || len(msgs) == 0
		lowest := before
		for _, m := range msgs {
			if m.ID <= after {
				done = true
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
			if lowest == 0 || m.ID < lowest {
				lowest = m.ID
			}
		}
		if done || lowest == before { // no progress: stop instead of looping
			break
		}
		before = lowest
	}
	slices.SortFunc(out, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// AppNames returns application names by ID.
func (c *Client) AppNames(ctx context.Context) (map[uint]string, error) {
	var apps []struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
	}
	if err := c.getJSON(ctx, "/application", nil, &apps); err != nil {
		return nil, err
	}
	names := make(map[uint]string, len(apps))
	for _, a := range apps {
		names[a.ID] = a.Name
	}
	return names, nil
}

// Stream is an open /stream WebSocket connection.
type Stream struct {
	conn *websocket.Conn
}

// Connect opens the /stream WebSocket.
func (c *Client) Connect(ctx context.Context) (*Stream, error) {
	u := *c.base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path += "/stream"

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPClient: c.ws,
		HTTPHeader: http.Header{tokenHeaderName: {c.token}},
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("connect stream: HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("connect stream: %w", err)
	}
	conn.SetReadLimit(maxStreamFrame)
	return &Stream{conn: conn}, nil
}

// Close closes the connection.
func (s *Stream) Close() { _ = s.conn.Close(websocket.StatusNormalClosure, "") }

// Read calls handle for every message until the connection fails or ctx
// is done. It pings the server periodically and calls alive after each
// answered ping; a missing pong closes the connection. handle may return
// an error to stop reading.
func (s *Stream) Read(ctx context.Context, alive func(), handle func(Message) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pingErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := s.conn.Ping(pctx)
			pcancel()
			if err != nil {
				if ctx.Err() == nil {
					pingErr <- fmt.Errorf("ping: %w", err)
					cancel() // unblocks Read
				}
				return
			}
			alive()
		}
	}()

	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			select {
			case perr := <-pingErr:
				return perr
			default:
			}
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read stream: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil || m.ID == 0 {
			continue // not a message (should not happen)
		}
		if err := handle(m); err != nil {
			return err
		}
	}
}
