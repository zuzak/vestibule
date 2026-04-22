package main

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// cachedResponse is a full captured upstream response held in memory so that
// calls inside the min_interval floor can be served without hitting upstream.
type cachedResponse struct {
	status  int
	headers map[string][]string
	body    []byte
}

// throttleEntry tracks the last upstream fetch for a (upstream, endpoint,
// canonical-params) key. Served responses share the entry so repeated
// identical calls inside the floor are cheap.
type throttleEntry struct {
	last   time.Time
	result *cachedResponse
}

type throttle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
	now     func() time.Time
}

func newThrottle() *throttle {
	return &throttle{
		entries: make(map[string]*throttleEntry),
		now:     time.Now,
	}
}

// canonicalKey renders a stable key for a given upstream+endpoint+params
// combination. Params are sorted so ?a=1&b=2 and ?b=2&a=1 match.
func canonicalKey(upstream, endpoint string, params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(upstream)
	b.WriteByte('\x00')
	b.WriteString(endpoint)
	for _, k := range keys {
		values := params[k]
		sort.Strings(values)
		for _, v := range values {
			b.WriteByte('\x00')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(v)
		}
	}
	return b.String()
}

// check returns a cached response when the floor has not yet elapsed since the
// last fetch for this key.
func (t *throttle) check(key string, minInterval time.Duration) (*cachedResponse, bool) {
	if minInterval <= 0 {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	if !ok || entry.result == nil {
		return nil, false
	}
	if t.now().Sub(entry.last) < minInterval {
		return entry.result, true
	}
	return nil, false
}

// store records the response under key with the current time.
func (t *throttle) store(key string, result *cachedResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[key] = &throttleEntry{last: t.now(), result: result}
}
