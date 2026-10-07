package state

import (
	"testing"
	"time"
)

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Load(dir); err != nil || ok {
		t.Fatalf("empty dir: ok=%v err=%v", ok, err)
	}
	if err := Save(dir, 42); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, 43); err != nil {
		t.Fatal(err)
	}
	id, ok, err := Load(dir)
	if err != nil || !ok || id != 43 {
		t.Fatalf("got id=%d ok=%v err=%v", id, ok, err)
	}
}

func TestHealth(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	if CheckHealth(dir, now, time.Minute) == nil {
		t.Fatal("missing file must be unhealthy")
	}
	if err := Touch(dir, now); err != nil {
		t.Fatal(err)
	}
	if err := CheckHealth(dir, now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if CheckHealth(dir, now.Add(2*time.Minute), time.Minute) == nil {
		t.Fatal("stale must be unhealthy")
	}
}
