package cache

import (
	"testing"
	"time"
)

func TestPutGetRoundTrip(t *testing.T) {
	s := New(t.TempDir(), "profile-a", "ap-northeast-1")
	if err := Put(s, "k", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	v, at, ok := Get[[]string](s, "k", time.Hour)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(v) != 2 || v[0] != "a" {
		t.Fatalf("got %v", v)
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("bogus fetch time %v", at)
	}
}

func TestGetExpired(t *testing.T) {
	s := New(t.TempDir(), "p", "r")
	if err := Put(s, "k", 42); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Get[int](s, "k", 0); ok {
		t.Fatal("expected expiry with ttl=0")
	}
}

func TestProfileScopeIsolation(t *testing.T) {
	base := t.TempDir()
	profileA := New(base, "profile-a", "ap-northeast-1")
	profileB := New(base, "profile-b", "ap-northeast-1")
	if err := Put(profileA, "accounts", []string{"profile-a-data"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Get[[]string](profileB, "accounts", time.Hour); ok {
		t.Fatal("a profile scope must never see another profile's cache entries")
	}
}

func TestClearAndEntries(t *testing.T) {
	s := New(t.TempDir(), "p", "r")
	_ = Put(s, "a", 1)
	_ = Put(s, "b", 2)
	entries, err := s.Entries()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.Entries()
	if len(entries) != 0 {
		t.Fatalf("expected empty after clear, got %v", entries)
	}
}
