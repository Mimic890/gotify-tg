// Package config loads and validates the runtime configuration from
// environment variables and secret files.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// maxSecretFileSize bounds how much is read from a *_FILE secret.
const maxSecretFileSize = 4096

var telegramTokenRe = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{30,64}$`)

// Config is the validated runtime configuration.
type Config struct {
	GotifyURL     *url.URL // base URL, scheme http or https, no trailing slash
	GotifyToken   string
	TelegramToken string
	ChatID        int64
	MinPriority   int
	AppIDs        map[uint]bool // nil means all applications
	DataDir       string
	LogLevel      slog.Level
}

// Load reads the configuration using getenv (usually os.Getenv) and
// readFile (usually os.ReadFile). All problems are reported together.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	var errs []error
	c := &Config{DataDir: "/data"}

	insecure, err := parseBool(getenv("ALLOW_INSECURE_GOTIFY"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ALLOW_INSECURE_GOTIFY: %w", err))
	}

	c.GotifyURL, err = parseGotifyURL(getenv("GOTIFY_URL"), insecure)
	if err != nil {
		errs = append(errs, fmt.Errorf("GOTIFY_URL: %w", err))
	}

	c.GotifyToken, err = secret(getenv, readFile, "GOTIFY_CLIENT_TOKEN")
	if err == nil {
		err = validateGotifyToken(c.GotifyToken)
	}
	if err != nil {
		errs = append(errs, err)
	}

	c.TelegramToken, err = secret(getenv, readFile, "TELEGRAM_BOT_TOKEN")
	if err == nil && !telegramTokenRe.MatchString(c.TelegramToken) {
		err = errors.New("TELEGRAM_BOT_TOKEN: invalid format, expected <bot id>:<secret> as issued by @BotFather")
	}
	if err != nil {
		errs = append(errs, err)
	}

	if v := strings.TrimSpace(getenv("TELEGRAM_CHAT_ID")); v == "" {
		errs = append(errs, errors.New("TELEGRAM_CHAT_ID: required"))
	} else if id, err := strconv.ParseInt(v, 10, 64); err != nil || id == 0 {
		errs = append(errs, fmt.Errorf("TELEGRAM_CHAT_ID: %q is not a non-zero integer", v))
	} else {
		c.ChatID = id
	}

	if v := strings.TrimSpace(getenv("MIN_PRIORITY")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 0 || p > 1000 {
			errs = append(errs, fmt.Errorf("MIN_PRIORITY: %q is not an integer between 0 and 1000", v))
		}
		c.MinPriority = p
	}

	c.AppIDs, err = parseAppIDs(getenv("APP_IDS"))
	if err != nil {
		errs = append(errs, fmt.Errorf("APP_IDS: %w", err))
	}

	if v := strings.TrimSpace(getenv("DATA_DIR")); v != "" {
		if !strings.HasPrefix(v, "/") {
			errs = append(errs, fmt.Errorf("DATA_DIR: %q must be an absolute path", v))
		}
		c.DataDir = v
	}

	if v := strings.TrimSpace(getenv("LOG_LEVEL")); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %q is not one of debug, info, warn, error", v))
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// Allowed reports whether a message passes the priority and app filters.
func (c *Config) Allowed(appID uint, priority int) bool {
	if priority < c.MinPriority {
		return false
	}
	return c.AppIDs == nil || c.AppIDs[appID]
}

// secret reads NAME_FILE (preferred) or NAME. Setting both is an error.
func secret(getenv func(string) string, readFile func(string) ([]byte, error), name string) (string, error) {
	file := strings.TrimSpace(getenv(name + "_FILE"))
	plain := getenv(name)
	switch {
	case file != "" && plain != "":
		return "", fmt.Errorf("%s: set only one of %s_FILE or %s", name, name, name)
	case file != "":
		b, err := readFile(file)
		if err != nil {
			// The path is not secret; the content is never included.
			return "", fmt.Errorf("%s_FILE: cannot read %q: %w", name, file, unwrapPathError(err))
		}
		if len(b) > maxSecretFileSize {
			return "", fmt.Errorf("%s_FILE: %q is larger than %d bytes", name, file, maxSecretFileSize)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("%s_FILE: %q is empty", name, file)
		}
		return v, nil
	case plain != "":
		return strings.TrimSpace(plain), nil
	default:
		return "", fmt.Errorf("%s: required, set %s_FILE (recommended) or %s", name, name, name)
	}
}

func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func validateGotifyToken(t string) error {
	if len(t) > 512 {
		return errors.New("GOTIFY_CLIENT_TOKEN: too long")
	}
	for _, r := range t {
		if r <= ' ' || r > '~' {
			return errors.New("GOTIFY_CLIENT_TOKEN: contains whitespace or non-printable characters")
		}
	}
	return nil
}

func parseGotifyURL(raw string, insecure bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("not a valid URL")
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "https"
	case "http", "ws":
		if !insecure {
			return nil, errors.New("plain http/ws is refused; use https/wss or set ALLOW_INSECURE_GOTIFY=true")
		}
		u.Scheme = "http"
	default:
		return nil, fmt.Errorf("unsupported scheme %q, expected https or wss", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if u.User != nil {
		return nil, errors.New("must not contain credentials, use GOTIFY_CLIENT_TOKEN_FILE")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func parseAppIDs(raw string) (map[uint]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	ids := map[uint]bool{}
	for f := range strings.SplitSeq(raw, ",") {
		f = strings.TrimSpace(f)
		id, err := strconv.ParseUint(f, 10, 32)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("%q is not a positive integer", f)
		}
		ids[uint(id)] = true
	}
	return ids, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "0", "no":
		return false, nil
	case "true", "1", "yes":
		return true, nil
	}
	return false, fmt.Errorf("%q is not a boolean (true/false)", v)
}
