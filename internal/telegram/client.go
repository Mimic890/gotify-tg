// Package telegram sends messages through the Telegram Bot API. It only
// calls sendMessage; the bot never receives updates.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseSize = 1 << 20
	maxBackoff      = 60 * time.Second
)

// ErrRejected is returned when Telegram refuses a message for a reason that
// retrying cannot fix (HTTP 400).
var ErrRejected = errors.New("telegram rejected the message")

// Client sends messages to a single chat.
type Client struct {
	BaseURL string // https://api.telegram.org, overridable in tests
	HTTP    *http.Client
	Log     *slog.Logger

	token  string
	chatID int64
}

// New returns a client for the given bot token and chat.
func New(httpClient *http.Client, token string, chatID int64, log *slog.Logger) *Client {
	return &Client{
		BaseURL: "https://api.telegram.org",
		HTTP:    httpClient,
		Log:     log,
		token:   token,
		chatID:  chatID,
	}
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Send delivers text (Telegram HTML). It retries on rate limits, server
// errors and network errors until ctx is done, and returns ErrRejected
// (wrapped) for permanent failures.
func (c *Client) Send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":              c.chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	})
	if err != nil {
		return err
	}

	backoff := time.Second
	for attempt := 1; ; attempt++ {
		wait, err := c.sendOnce(ctx, body)
		if err == nil || errors.Is(err, ErrRejected) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if wait == 0 {
			wait = backoff/2 + rand.N(backoff/2+1)
			backoff = min(backoff*2, maxBackoff)
		}
		c.Log.Warn("telegram send failed, retrying", "attempt", attempt, "retry_in", wait.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// sendOnce performs one request. A non-zero wait means Telegram asked us
// to wait that long before retrying.
func (c *Client) sendOnce(ctx context.Context, body []byte) (wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, c.redact(err)
	}
	defer resp.Body.Close()

	var r apiResponse
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return 0, c.redact(err)
	}
	_ = json.Unmarshal(data, &r) // a broken body is handled via the status code

	switch {
	case resp.StatusCode == http.StatusOK && r.OK:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := min(max(r.Parameters.RetryAfter, 1), 3600)
		return time.Duration(ra) * time.Second, fmt.Errorf("rate limited (retry_after=%ds)", ra)
	case resp.StatusCode == http.StatusBadRequest:
		return 0, fmt.Errorf("%w: %s", ErrRejected, c.redactString(r.Description))
	default:
		// 5xx, and 401/403/404 which usually mean a configuration problem
		// (revoked token, bot blocked by the user). Keep retrying so no
		// message is lost once it is fixed.
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, c.redactString(r.Description))
	}
}

// redact strips the request URL (which contains the bot token) from
// transport errors and removes any remaining occurrence of the token.
func (c *Client) redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = fmt.Errorf("%s request: %w", ue.Op, ue.Err)
	}
	return errors.New(c.redactString(err.Error()))
}

func (c *Client) redactString(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[REDACTED]")
}
