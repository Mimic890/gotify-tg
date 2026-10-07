package config

import (
	"os"
	"strings"
	"testing"
)

const tgToken = "123456789:AAH-abcdefghijklmnopqrstuvwxyz_0123"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
}

func valid() map[string]string {
	return map[string]string{
		"GOTIFY_URL":               "https://push.example.com/gotify/",
		"GOTIFY_CLIENT_TOKEN_FILE": "/run/secrets/gotify",
		"TELEGRAM_BOT_TOKEN_FILE":  "/run/secrets/tg",
		"TELEGRAM_CHAT_ID":         "123456",
	}
}

var secretFiles = map[string]string{
	"/run/secrets/gotify": "CsecretGotify\n",
	"/run/secrets/tg":     tgToken + "\n",
}

func TestLoadValid(t *testing.T) {
	m := valid()
	m["MIN_PRIORITY"] = "5"
	m["APP_IDS"] = "1, 3"
	m["LOG_LEVEL"] = "debug"
	c, err := Load(env(m), files(secretFiles))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GotifyURL.String(); got != "https://push.example.com/gotify" {
		t.Errorf("url = %q", got)
	}
	if c.GotifyToken != "CsecretGotify" || c.TelegramToken != tgToken {
		t.Error("secrets not trimmed/read")
	}
	if c.ChatID != 123456 || c.MinPriority != 5 || !c.AppIDs[1] || !c.AppIDs[3] || len(c.AppIDs) != 2 {
		t.Errorf("unexpected config %+v", c)
	}
	if c.DataDir != "/data" {
		t.Errorf("data dir = %q", c.DataDir)
	}
	if !c.Allowed(3, 5) || c.Allowed(2, 9) || c.Allowed(1, 4) {
		t.Error("filter mismatch")
	}
}

func TestLoadDefaultsAllowAll(t *testing.T) {
	c, err := Load(env(valid()), files(secretFiles))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppIDs != nil || c.MinPriority != 0 || !c.Allowed(42, 0) {
		t.Error("defaults should allow everything")
	}
}

func TestLoadPlainEnvFallback(t *testing.T) {
	m := valid()
	delete(m, "GOTIFY_CLIENT_TOKEN_FILE")
	m["GOTIFY_CLIENT_TOKEN"] = "Cplain"
	c, err := Load(env(m), files(secretFiles))
	if err != nil {
		t.Fatal(err)
	}
	if c.GotifyToken != "Cplain" {
		t.Errorf("token = %q", c.GotifyToken)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		mod  func(m map[string]string)
		want string
	}{
		{"missing url", func(m map[string]string) { delete(m, "GOTIFY_URL") }, "GOTIFY_URL: required"},
		{"http refused", func(m map[string]string) { m["GOTIFY_URL"] = "http://gotify:80" }, "ALLOW_INSECURE_GOTIFY"},
		{"ws refused", func(m map[string]string) { m["GOTIFY_URL"] = "ws://gotify" }, "ALLOW_INSECURE_GOTIFY"},
		{"bad scheme", func(m map[string]string) { m["GOTIFY_URL"] = "ftp://x" }, "unsupported scheme"},
		{"credentials in url", func(m map[string]string) { m["GOTIFY_URL"] = "https://u:p@x" }, "credentials"},
		{"query in url", func(m map[string]string) { m["GOTIFY_URL"] = "https://x/?token=abc" }, "query"},
		{"bad insecure flag", func(m map[string]string) { m["ALLOW_INSECURE_GOTIFY"] = "maybe" }, "ALLOW_INSECURE_GOTIFY"},
		{"both token sources", func(m map[string]string) { m["GOTIFY_CLIENT_TOKEN"] = "x" }, "set only one"},
		{"missing token", func(m map[string]string) { delete(m, "GOTIFY_CLIENT_TOKEN_FILE") }, "GOTIFY_CLIENT_TOKEN: required"},
		{"unreadable file", func(m map[string]string) { m["TELEGRAM_BOT_TOKEN_FILE"] = "/nope" }, "cannot read"},
		{"bad tg token", func(m map[string]string) {
			delete(m, "TELEGRAM_BOT_TOKEN_FILE")
			m["TELEGRAM_BOT_TOKEN"] = "nonsense"
		}, "invalid format"},
		{"missing chat", func(m map[string]string) { delete(m, "TELEGRAM_CHAT_ID") }, "TELEGRAM_CHAT_ID: required"},
		{"chat not int", func(m map[string]string) { m["TELEGRAM_CHAT_ID"] = "@me" }, "TELEGRAM_CHAT_ID"},
		{"chat zero", func(m map[string]string) { m["TELEGRAM_CHAT_ID"] = "0" }, "TELEGRAM_CHAT_ID"},
		{"priority", func(m map[string]string) { m["MIN_PRIORITY"] = "-1" }, "MIN_PRIORITY"},
		{"app ids", func(m map[string]string) { m["APP_IDS"] = "1,x" }, "APP_IDS"},
		{"app id zero", func(m map[string]string) { m["APP_IDS"] = "0" }, "APP_IDS"},
		{"relative data dir", func(m map[string]string) { m["DATA_DIR"] = "data" }, "DATA_DIR"},
		{"log level", func(m map[string]string) { m["LOG_LEVEL"] = "loud" }, "LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid()
			tt.mod(m)
			_, err := Load(env(m), files(secretFiles))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestInsecureAllowed(t *testing.T) {
	m := valid()
	m["GOTIFY_URL"] = "ws://gotify:80"
	m["ALLOW_INSECURE_GOTIFY"] = "true"
	c, err := Load(env(m), files(secretFiles))
	if err != nil {
		t.Fatal(err)
	}
	if c.GotifyURL.Scheme != "http" {
		t.Errorf("scheme = %q", c.GotifyURL.Scheme)
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	m := valid()
	m["TELEGRAM_CHAT_ID"] = "bad"
	delete(m, "TELEGRAM_BOT_TOKEN_FILE")
	m["TELEGRAM_BOT_TOKEN"] = "123:short-secret-value"
	_, err := Load(env(m), files(secretFiles))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range []string{"short-secret-value", "CsecretGotify"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaks secret %q: %v", s, err)
		}
	}
}
