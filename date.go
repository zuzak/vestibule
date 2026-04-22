package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// maxDateOffsetDays bounds the signed offset accepted by today±N. Generous
// enough for legitimate archival use, tight enough that a malformed large
// integer fails fast instead of quietly producing a year-3000 date.
const maxDateOffsetDays = 3650

var todayOffsetRE = regexp.MustCompile(`^today([+-])(\d+)$`)

// resolveDate turns a user-supplied date param value into a canonical
// YYYY-MM-DD string. Accepted inputs: YYYY-MM-DD, today, yesterday,
// tomorrow, today+N, today-N (N a non-negative integer ≤ maxDateOffsetDays).
// Resolution uses UTC. now is injectable for testing.
func resolveDate(value string, now func() time.Time) (string, error) {
	switch value {
	case "today":
		return today(now), nil
	case "yesterday":
		return offsetDays(now, -1), nil
	case "tomorrow":
		return offsetDays(now, 1), nil
	}

	if m := todayOffsetRE.FindStringSubmatch(value); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", dateError(value)
		}
		if n > maxDateOffsetDays {
			return "", dateError(value)
		}
		if m[1] == "-" {
			n = -n
		}
		return offsetDays(now, n), nil
	}

	// Literal date: must parse as YYYY-MM-DD and round-trip identically.
	t, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		return "", dateError(value)
	}
	// time.Parse tolerates some oddities (e.g. "2026-4-22"); require exact.
	if t.Format("2006-01-02") != value {
		return "", dateError(value)
	}
	return value, nil
}

func today(now func() time.Time) string {
	return now().UTC().Format("2006-01-02")
}

func offsetDays(now func() time.Time, n int) string {
	return now().UTC().AddDate(0, 0, n).Format("2006-01-02")
}

func dateError(value string) error {
	return fmt.Errorf("invalid date %q: expected YYYY-MM-DD, today, yesterday, tomorrow, or today±N (N ≤ %d)", value, maxDateOffsetDays)
}
