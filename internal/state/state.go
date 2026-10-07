// Package state persists the last forwarded message ID and the
// health timestamp in the data directory.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	stateFile  = "state.json"
	healthFile = "last_connected"
)

type fileData struct {
	LastID uint `json:"last_id"`
}

// Load returns the last forwarded message ID. ok is false when no state
// has been saved yet.
func Load(dir string) (lastID uint, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", stateFile, err)
	}
	return d.LastID, true, nil
}

// Save atomically writes the last forwarded message ID.
func Save(dir string, lastID uint) error {
	b, _ := json.Marshal(fileData{LastID: lastID})
	return writeAtomic(dir, stateFile, append(b, '\n'))
}

// Touch records that the stream is connected at time now.
func Touch(dir string, now time.Time) error {
	return writeAtomic(dir, healthFile, []byte(strconv.FormatInt(now.Unix(), 10)+"\n"))
}

// CheckHealth returns an error unless Touch was called within maxAge.
func CheckHealth(dir string, now time.Time, maxAge time.Duration) error {
	b, err := os.ReadFile(filepath.Join(dir, healthFile))
	if err != nil {
		return err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse %s: %w", healthFile, err)
	}
	if age := now.Sub(time.Unix(ts, 0)); age > maxAge {
		return fmt.Errorf("stream last confirmed alive %s ago", age.Round(time.Second))
	}
	return nil
}

func writeAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
