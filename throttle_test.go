package main

import (
	"net/url"
	"testing"
	"time"
)

func TestThrottleFloor(t *testing.T) {
	tr := newThrottle()
	now := time.Unix(1_000_000, 0)
	tr.now = func() time.Time { return now }

	key := canonicalKey("demo", "things", url.Values{"date": {"2026-04-22"}})
	resp := &cachedResponse{status: 200, body: []byte(`{"ok":true}`)}

	// First check: empty.
	if _, hit := tr.check(key, 30*time.Second); hit {
		t.Fatal("expected miss on empty throttle")
	}

	tr.store(key, resp)

	// Immediately after store: within floor, hit.
	if cached, hit := tr.check(key, 30*time.Second); !hit {
		t.Fatal("expected hit inside min_interval")
	} else if string(cached.body) != `{"ok":true}` {
		t.Errorf("cached body: got %q", cached.body)
	}

	// Advance past the floor.
	now = now.Add(31 * time.Second)
	if _, hit := tr.check(key, 30*time.Second); hit {
		t.Fatal("expected miss after floor elapsed")
	}
}

func TestThrottleKeySeparation(t *testing.T) {
	tr := newThrottle()
	now := time.Unix(1_000_000, 0)
	tr.now = func() time.Time { return now }

	keyA := canonicalKey("demo", "things", url.Values{"date": {"2026-04-22"}})
	keyB := canonicalKey("demo", "things", url.Values{"date": {"2026-04-23"}})
	if keyA == keyB {
		t.Fatal("keys for different params collided")
	}

	tr.store(keyA, &cachedResponse{status: 200, body: []byte("A")})
	if _, hit := tr.check(keyB, 30*time.Second); hit {
		t.Error("different-param key should not hit on A's cache")
	}
	if _, hit := tr.check(keyA, 30*time.Second); !hit {
		t.Error("same-param key should hit")
	}
}

func TestCanonicalKeyStable(t *testing.T) {
	k1 := canonicalKey("demo", "things", url.Values{"a": {"1"}, "b": {"2"}})
	k2 := canonicalKey("demo", "things", url.Values{"b": {"2"}, "a": {"1"}})
	if k1 != k2 {
		t.Errorf("canonical key should be stable across map iteration order: %q vs %q", k1, k2)
	}
}

func TestThrottleZeroInterval(t *testing.T) {
	tr := newThrottle()
	key := canonicalKey("demo", "things", nil)
	tr.store(key, &cachedResponse{status: 200})
	if _, hit := tr.check(key, 0); hit {
		t.Error("min_interval=0 must never hit cache")
	}
}
