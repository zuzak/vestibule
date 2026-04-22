package main

import (
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock stuck at the given UTC date.
func fixedClock(date string) func() time.Time {
	t, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		panic(err)
	}
	// Use midday so any accidental DST-adjacent tests don't bite; the
	// resolver uses UTC explicitly so this is belt-and-braces.
	t = t.Add(12 * time.Hour)
	return func() time.Time { return t }
}

func TestResolveDateAliases(t *testing.T) {
	now := fixedClock("2026-04-22")
	cases := []struct {
		input string
		want  string
	}{
		{"today", "2026-04-22"},
		{"yesterday", "2026-04-21"},
		{"tomorrow", "2026-04-23"},
		{"today-0", "2026-04-22"},
		{"today+0", "2026-04-22"},
		{"today-7", "2026-04-15"},
		{"today+3", "2026-04-25"},
		{"today-30", "2026-03-23"},
		{"2025-12-31", "2025-12-31"},
		{"2026-04-22", "2026-04-22"},
	}
	for _, tc := range cases {
		got, err := resolveDate(tc.input, now)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestResolveDateRejections(t *testing.T) {
	now := fixedClock("2026-04-22")
	cases := []string{
		"",
		"yester-day",
		"2026-4-22",   // missing leading zero
		"2026/04/22",  // wrong separator
		"2026-13-01",  // invalid month
		"today-3651",  // out of range
		"today+99999", // out of range
		"today-",      // missing N
		"today-abc",   // non-integer N
		"not-a-date",
	}
	for _, v := range cases {
		if _, err := resolveDate(v, now); err == nil {
			t.Errorf("%q: expected rejection, got nil", v)
		}
	}
}

// TestResolveDateUsesUTC: the clock returns a moment that is late-at-night
// UTC but early-next-day in most tz-ahead zones. resolveDate must use UTC.
func TestResolveDateUsesUTC(t *testing.T) {
	// 2026-04-22 23:59 UTC → in UTC, "today" is still the 22nd.
	t22 := time.Date(2026, 4, 22, 23, 59, 0, 0, time.UTC)
	now := func() time.Time { return t22 }
	got, err := resolveDate("today", now)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-04-22" {
		t.Errorf("UTC today: got %q, want 2026-04-22", got)
	}
}

// TestResolveDateErrorMessage: the error must describe the allowed forms so
// a caller can correct their request.
func TestResolveDateErrorMessage(t *testing.T) {
	now := fixedClock("2026-04-22")
	_, err := resolveDate("nope", now)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"YYYY-MM-DD", "today", "yesterday", "tomorrow"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing mention of %q", msg, want)
		}
	}
}
