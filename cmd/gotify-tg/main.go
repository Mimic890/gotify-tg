// Command gotify-tg forwards Gotify messages to a Telegram chat.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mimic890/gotify-tg/internal/config"
	"github.com/mimic890/gotify-tg/internal/gotify"
	"github.com/mimic890/gotify-tg/internal/state"
	"github.com/mimic890/gotify-tg/internal/telegram"
)

const (
	queueSize       = 1000
	drainTimeout    = 15 * time.Second
	maxReconnect    = 60 * time.Second
	healthMaxAge    = 90 * time.Second
	appNamesRefresh = time.Minute
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [healthcheck]\n", os.Args[0])
		os.Exit(2)
	}

	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := newBridge(cfg, log).run(ctx); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

func healthcheck() int {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	if err := state.CheckHealth(dir, time.Now(), healthMaxAge); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	return 0
}

type bridge struct {
	cfg    *config.Config
	log    *slog.Logger
	gotify *gotify.Client
	tg     *telegram.Client
	queue  chan gotify.Message

	lastQueued uint // reader goroutine only

	appNames     map[uint]string // sender goroutine only
	appNamesTime time.Time
}

var errQueueFull = errors.New("send queue full")

func newBridge(cfg *config.Config, log *slog.Logger) *bridge {
	return &bridge{
		cfg:    cfg,
		log:    log,
		gotify: gotify.New(cfg.GotifyURL, cfg.GotifyToken, restClient(), wsClient()),
		tg:     telegram.New(restClient(), cfg.TelegramToken, cfg.ChatID, log),
		queue:  make(chan gotify.Message, queueSize),
	}
}

func (b *bridge) run(ctx context.Context) error {
	cfg, log := b.cfg, b.log
	lastID, ok, err := state.Load(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !ok {
		// First start: begin with the newest existing message instead of
		// replaying the whole history.
		lastID, err = b.latestID(ctx)
		if err != nil {
			return err
		}
		if err := state.Save(cfg.DataDir, lastID); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		log.Info("no saved state, starting after the newest message", "last_id", lastID)
	}
	b.lastQueued = lastID

	log.Info("starting", "gotify_host", cfg.GotifyURL.Host, "last_id", lastID,
		"min_priority", cfg.MinPriority, "app_filter", len(cfg.AppIDs) > 0)

	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		b.sender(sendCtx)
	}()

	b.readLoop(ctx)

	// Shutdown: stop accepting, give the sender a bounded time to drain.
	close(b.queue)
	log.Info("shutting down, draining queue", "pending", len(b.queue))
	select {
	case <-senderDone:
	case <-time.After(drainTimeout):
		log.Warn("drain timeout, unsent messages will be sent after restart", "pending", len(b.queue))
		cancelSend()
		<-senderDone
	}
	return nil
}

// latestID asks Gotify for the newest message ID, retrying until it works.
func (b *bridge) latestID(ctx context.Context) (uint, error) {
	backoff := time.Second
	for {
		id, err := b.gotify.LatestID(ctx)
		if err == nil {
			return id, nil
		}
		b.log.Warn("cannot fetch latest message ID", "err", err)
		if err := sleep(ctx, jitter(backoff)); err != nil {
			return 0, err
		}
		backoff = min(backoff*2, maxReconnect)
	}
}

// readLoop keeps a stream session open until ctx is done.
func (b *bridge) readLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := b.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		wait := jitter(backoff)
		b.log.Warn("stream disconnected, reconnecting", "err", err, "retry_in", wait.String())
		if sleep(ctx, wait) != nil {
			return
		}
		backoff = min(backoff*2, maxReconnect)
	}
}

// session connects, catches up on missed messages and reads the stream.
// The stream is opened before the catch-up so that nothing published in
// between is missed; duplicates are dropped by ID.
func (b *bridge) session(ctx context.Context) (connected bool, err error) {
	s, err := b.gotify.Connect(ctx)
	if err != nil {
		return false, err
	}
	defer s.Close()
	b.touch()
	b.log.Info("stream connected")

	missed, err := b.gotify.MessagesAfter(ctx, b.lastQueued)
	if err != nil {
		return true, fmt.Errorf("catch-up: %w", err)
	}
	if len(missed) > 0 {
		b.log.Info("catching up", "count", len(missed), "after_id", b.lastQueued)
	}
	for _, m := range missed {
		if err := b.offer(ctx, m, true); err != nil {
			return true, err
		}
	}
	return true, s.Read(ctx, b.touch, func(m gotify.Message) error {
		return b.offer(ctx, m, false)
	})
}

// offer queues m unless it was already queued or is filtered out. With
// block=false a full queue aborts the session; the reconnect then picks the
// message up again through the catch-up.
func (b *bridge) offer(ctx context.Context, m gotify.Message, block bool) error {
	if m.ID <= b.lastQueued {
		b.log.Debug("skipping duplicate", "id", m.ID)
		return nil
	}
	if !b.cfg.Allowed(m.AppID, m.Prio()) {
		b.log.Debug("filtered", "id", m.ID, "app_id", m.AppID, "priority", m.Prio())
		b.lastQueued = m.ID
		return nil
	}
	if block {
		select {
		case b.queue <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		select {
		case b.queue <- m:
		default:
			return errQueueFull
		}
	}
	b.lastQueued = m.ID
	return nil
}

func (b *bridge) sender(ctx context.Context) {
	for m := range b.queue {
		chunks := telegram.Format(m.Title, b.appName(ctx, m.AppID), m.Message)
		for _, c := range chunks {
			err := b.tg.Send(ctx, c)
			if ctx.Err() != nil {
				return // not saved: will be sent again after restart
			}
			if err != nil {
				b.log.Error("message dropped", "id", m.ID, "err", err)
				break
			}
		}
		if err := state.Save(b.cfg.DataDir, m.ID); err != nil {
			b.log.Error("save state", "err", err)
		}
		b.log.Debug("forwarded", "id", m.ID, "parts", len(chunks))
	}
}

func (b *bridge) appName(ctx context.Context, id uint) string {
	if name, ok := b.appNames[id]; ok {
		return name
	}
	if time.Since(b.appNamesTime) > appNamesRefresh {
		b.appNamesTime = time.Now()
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		names, err := b.gotify.AppNames(rctx)
		cancel()
		if err != nil {
			b.log.Warn("cannot fetch application names", "err", err)
		} else {
			b.appNames = names
		}
	}
	if name, ok := b.appNames[id]; ok {
		return name
	}
	return fmt.Sprintf("app #%d", id)
}

func (b *bridge) touch() {
	if err := state.Touch(b.cfg.DataDir, time.Now()); err != nil {
		b.log.Warn("cannot write health file", "err", err)
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func baseTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// restClient is used for short REST calls; redirects are not followed so
// credentials are never sent to another location.
func restClient() *http.Client {
	t := baseTransport()
	t.ForceAttemptHTTP2 = true
	return &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: noRedirect}
}

// wsClient is used for the long-lived WebSocket. It has no overall timeout
// (the dial uses a context deadline) and speaks HTTP/1.1 only.
func wsClient() *http.Client {
	return &http.Client{Transport: baseTransport(), CheckRedirect: noRedirect}
}
