package proxy

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- key rotation state ---
//
// Strategy: sticky exhaustion. Traffic stays on ONE key (the last one that
// succeeded) until its limits are burned out, then moves to the next key with
// quota left, burns it out too, and so on. Uniform round-robin is deliberately
// NOT used: flipping keys on every request defeats upstream keep-alive/prompt
// caching, which is bound to a single key session.
//
// A key that hits its usage limit (429) is put into cooldown for the whole
// reset window (hours / week / month, parsed from the upstream "Resets in N"
// hint) so it is not re-probed while exhausted. A cooled key is skipped while
// it cools, probed again once the deadline passes (limits may reset), and
// forgotten entirely on success — a key is never permanently removed, only
// deprioritized behind the current burning key.

var rotationState = struct {
	mu       sync.Mutex
	lastGood map[string]string // family -> provider Name
}{lastGood: map[string]string{}}

// markKeySuccess clears the provider's cooldown and records it as the
// family's last successful key so the next request starts after it.
func markKeySuccess(family string, p *Provider) {
	p.cooldownUntil.Store(0)
	rotationState.mu.Lock()
	rotationState.lastGood[family] = p.Name
	rotationState.mu.Unlock()
}

// setCooldown deprioritizes p for the duration cooldownFor computes from the
// failure (429 honors Retry-After / "Resets in N", auth errors cool longest,
// bad requests and transport errors cool briefly so a key that recovers
// quickly returns to rotation almost immediately).
func setCooldown(p *Provider, status int, retryAfter string, body []byte) {
	d := cooldownFor(status, retryAfter, body)
	deadline := time.Now().Add(d).UnixNano()
	if until := p.cooldownUntil.Load(); until > deadline {
		// Existing cooldown already expires later — keep it.
		return
	}
	p.cooldownUntil.Store(deadline)
}

// buildAttemptOrder returns providers in try-order: the last successful key
// FIRST (sticky — all traffic stays on it while it has quota), followed by
// the other non-cooling keys, then the keys still cooling down. A key only
// loses the lead when it is exhausted (429/limit), at which point the next
// available key becomes the lead and is burned out too. If every key is
// cooling down, all are returned as-is — cooldowns are hints, not hard
// locks, and a stale cooldown must not brick the whole family.
func buildAttemptOrder(family string, providers []*Provider) []*Provider {
	rotationState.mu.Lock()
	lastGood := rotationState.lastGood[family]
	rotationState.mu.Unlock()

	now := time.Now().UnixNano()
	good := make([]*Provider, 0, len(providers))
	cooling := make([]*Provider, 0, len(providers))
	for _, p := range providers {
		if until := p.cooldownUntil.Load(); until > now {
			cooling = append(cooling, p)
		} else {
			good = append(good, p)
		}
	}
	if len(good) == 0 {
		return providers
	}
	idx := 0
	for i, p := range good {
		if p.Name == lastGood {
			idx = i
			break
		}
	}
	rotated := make([]*Provider, 0, len(good))
	rotated = append(rotated, good[idx:]...)
	rotated = append(rotated, good[:idx]...)
	return append(rotated, cooling...)
}

// --- cooldown durations ---
//
// Windows follow the upstream limit shapes: "several hours" (e.g. MiniMax
// 5h), a week, a month. A 429 puts the key out of rotation until the whole
// window resets, so an exhausted key is never re-probed while another key
// still has quota. cooldownMax is only a safety cap against absurd hints.

const (
	cooldown429Default = 6 * time.Hour
	cooldownAuth       = 30 * time.Minute
	cooldownBadRequest = 30 * time.Second
	cooldownTransport  = 5 * time.Second
	cooldownMax        = 45 * 24 * time.Hour
)

// resetsInRe matches upstream limit messages like "Resets in 23hr 60min",
// "Resets in 1 day", "Resets in 2 weeks" or "Resets in 1 month" so a key
// with a known reset time can cool for the whole window instead of being
// re-probed while still exhausted.
var resetsInRe = regexp.MustCompile(`(?i)resets? in (\d+)\s*(hr|hour|min|minute|day|week|month|h|m|d|w|mo)`)

// cooldownFor picks how long a provider stays deprioritized after a failed
// attempt. 429 honors Retry-After and the upstream "Resets in N" hint when
// present (capped by cooldownMax), auth errors cool longest, bad requests
// and transport errors cool briefly so a key that recovers quickly returns
// to rotation almost immediately.
func cooldownFor(status int, retryAfter string, body []byte) time.Duration {
	if status == http.StatusTooManyRequests {
		if d := parseRetryAfter(retryAfter); d > 0 {
			return capCooldown(d)
		}
		if d := parseResetsIn(body); d > 0 {
			return capCooldown(d)
		}
		return cooldown429Default
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return cooldownAuth
	case http.StatusBadRequest:
		return cooldownBadRequest
	}
	return cooldownTransport
}

func capCooldown(d time.Duration) time.Duration {
	if d > cooldownMax {
		return cooldownMax
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func parseResetsIn(body []byte) time.Duration {
	m := resetsInRe.FindStringSubmatch(string(body))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	unit := strings.ToLower(m[2])
	switch unit {
	case "h", "hr", "hour":
		return time.Duration(n) * time.Hour
	case "d", "day":
		return time.Duration(n) * 24 * time.Hour
	case "w", "week":
		return time.Duration(n) * 7 * 24 * time.Hour
	case "mo", "month":
		return time.Duration(n) * 31 * 24 * time.Hour
	case "m", "min", "minute":
		return time.Duration(n) * time.Minute
	}
	return 0
}

// ResetRotationForTest clears all rotation state. Test-only.
func ResetRotationForTest() {
	rotationState.mu.Lock()
	rotationState.lastGood = map[string]string{}
	rotationState.mu.Unlock()
}
