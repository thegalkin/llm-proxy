package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildAttemptOrderNoLastGood(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
		{Name: "c", Family: "f"},
	}
	got := names(buildAttemptOrder("f", ps))
	want := []string{"a", "b", "c"}
	if !eq(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestBuildAttemptOrderSticksToLastGood(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
		{Name: "c", Family: "f"},
	}
	markKeySuccess("f", ps[1]) // last good = b
	got := names(buildAttemptOrder("f", ps))
	want := []string{"b", "c", "a"}
	if !eq(got, want) {
		t.Fatalf("order = %v, want %v (sticky: last good first)", got, want)
	}
}

// A recovered key must NOT steal the lead back from the key that is
// currently being burned — sticky keeps the last successful key in front.
func TestBuildAttemptOrderStickyAfterCooldownExpiry(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
	}
	// a exhausted (cooling), b took over as last good.
	ps[0].cooldownUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())
	markKeySuccess("f", ps[1])
	// a's cooldown expires — but b stays the lead.
	ps[0].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())
	got := names(buildAttemptOrder("f", ps))
	want := []string{"b", "a"}
	if !eq(got, want) {
		t.Fatalf("order = %v, want %v (recovered key must not steal the lead)", got, want)
	}
}

func TestBuildAttemptOrderSkipsCoolingToBack(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
		{Name: "c", Family: "f"},
	}
	ps[0].cooldownUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())
	got := names(buildAttemptOrder("f", ps))
	want := []string{"b", "c", "a"}
	if !eq(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestBuildAttemptOrderExpiredCooldownReturnsToRotation(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
	}
	ps[0].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())
	got := names(buildAttemptOrder("f", ps))
	want := []string{"a", "b"}
	if !eq(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestBuildAttemptOrderAllCoolingProbesEverything(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "a", Family: "f"},
		{Name: "b", Family: "f"},
	}
	until := time.Now().Add(5 * time.Minute).UnixNano()
	ps[0].cooldownUntil.Store(until)
	ps[1].cooldownUntil.Store(until)
	got := names(buildAttemptOrder("f", ps))
	if len(got) != 2 {
		t.Fatalf("all-cooling order = %v, want both providers probed", got)
	}
}

func TestMarkKeySuccessClearsCooldown(t *testing.T) {
	ResetRotationForTest()
	p := &Provider{Name: "a", Family: "f"}
	p.cooldownUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())
	markKeySuccess("f", p)
	if got := p.cooldownUntil.Load(); got != 0 {
		t.Fatalf("cooldown = %d, want 0 after success", got)
	}
	if got := rotationState.lastGood["f"]; got != "a" {
		t.Fatalf("lastGood = %q, want a", got)
	}
}

func TestSetCooldownSetsAndKeepsLonger(t *testing.T) {
	ResetRotationForTest()
	p := &Provider{Name: "a", Family: "f"}
	// 429 with Resets-in hint -> capped at cooldownMax.
	body := []byte(`{"type":"error","error":{"message":"Weekly usage limit reached. Resets in 23hr 60min."}}`)
	setCooldown(p, http.StatusTooManyRequests, "", body)
	first := p.cooldownUntil.Load()
	if first == 0 {
		t.Fatal("setCooldown(429) left cooldown at 0")
	}
	// Shorter cooldown must NOT shorten an existing one.
	setCooldown(p, 0, "", nil) // transport -> 5s
	if got := p.cooldownUntil.Load(); got != first {
		t.Fatalf("short cooldown overwrote longer one: %d -> %d", first, got)
	}
}

func TestCooldownForRetryAfterHeader(t *testing.T) {
	d := cooldownFor(http.StatusTooManyRequests, "300", nil)
	if d != 5*time.Minute {
		t.Fatalf("Retry-After=300 -> %v, want 5m", d)
	}
}

func TestCooldownForResetsInParsing(t *testing.T) {
	body := []byte(`{"type":"error","error":{"message":"Weekly usage limit reached. Resets in 23hr 60min."}}`)
	d := cooldownFor(http.StatusTooManyRequests, "", body)
	if d != 23*time.Hour {
		t.Fatalf("Resets in 23hr -> %v, want 23h (kept below cooldownMax)", d)
	}
}

func TestCooldownForResetsInWeeks(t *testing.T) {
	body := []byte(`{"error":{"message":"Monthly usage limit reached. Resets in 2 weeks"}}`)
	d := cooldownFor(http.StatusTooManyRequests, "", body)
	if d != 14*24*time.Hour {
		t.Fatalf("Resets in 2 weeks -> %v, want 14d", d)
	}
}

func TestCooldownForResetsInMonths(t *testing.T) {
	body := []byte(`{"error":{"message":"Resets in 1 month"}}`)
	d := cooldownFor(http.StatusTooManyRequests, "", body)
	if d != 31*24*time.Hour {
		t.Fatalf("Resets in 1 month -> %v, want 31d", d)
	}
}

func TestCooldownForResetsInCapsAtMax(t *testing.T) {
	body := []byte(`{"error":{"message":"Resets in 60 days"}}`)
	d := cooldownFor(http.StatusTooManyRequests, "", body)
	if d != cooldownMax {
		t.Fatalf("Resets in 60 days -> %v, want capped %v", d, cooldownMax)
	}
}

func TestCooldownForResetsInMinutes(t *testing.T) {
	body := []byte(`{"error":{"message":"Resets in 90 min"}}`)
	d := cooldownFor(http.StatusTooManyRequests, "", body)
	if d != 90*time.Minute {
		t.Fatalf("Resets in 90 min -> %v, want 90m", d)
	}
}

func TestCooldownForDefaults(t *testing.T) {
	if d := cooldownFor(http.StatusTooManyRequests, "", nil); d != cooldown429Default {
		t.Fatalf("429 no hints -> %v, want %v", d, cooldown429Default)
	}
	if d := cooldownFor(http.StatusUnauthorized, "", nil); d != cooldownAuth {
		t.Fatalf("401 -> %v, want %v", d, cooldownAuth)
	}
	if d := cooldownFor(http.StatusBadRequest, "", nil); d != cooldownBadRequest {
		t.Fatalf("400 -> %v, want %v", d, cooldownBadRequest)
	}
	if d := cooldownFor(0, "", nil); d != cooldownTransport {
		t.Fatalf("transport error -> %v, want %v", d, cooldownTransport)
	}
}

// The core regression test: a key that hit its limit is NOT probed again on
// the next request; the live key takes over immediately.
func TestForwardOpencodeGoSkipsCooledKey(t *testing.T) {
	ResetRotationForTest()
	var k1Hits, k2Hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer k1":
			atomic.AddInt32(&k1Hits, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached. Resets in 1 day."}}`))
		case "Bearer k2":
			atomic.AddInt32(&k2Hits, 1)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstreamSrv.Close()

	providers := []Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}

	// First request: k1 429s, k2 succeeds.
	rec1 := httptest.NewRecorder()
	ForwardOpencodeGo(rec1, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (body: %s)", rec1.Code, rec1.Body.String())
	}
	if got := atomic.LoadInt32(&k1Hits); got != 1 {
		t.Fatalf("k1 hits after first request = %d, want 1", got)
	}

	// Second request: k1 is cooling down — must NOT be probed.
	rec2 := httptest.NewRecorder()
	ForwardOpencodeGo(rec2, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
	if got := atomic.LoadInt32(&k1Hits); got != 1 {
		t.Fatalf("k1 hits after second request = %d, want 1 (cooled key must be skipped)", got)
	}
	if got := atomic.LoadInt32(&k2Hits); got != 2 {
		t.Fatalf("k2 hits = %d, want 2", got)
	}
}

// After the cooldown expires the key is probed again — never forgotten.
// With sticky rotation this only happens when the current lead is also
// exhausted (all keys cooling), so the recovered key re-enters rotation.
func TestForwardOpencodeGoExpiredCooldownReProbes(t *testing.T) {
	ResetRotationForTest()
	var k1Hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			atomic.AddInt32(&k1Hits, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"Resets in 30 min"}}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"Resets in 30 min"}}`))
	}))
	defer upstreamSrv.Close()

	providers := []Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}

	// Both keys exhausted on the first request -> both cool down.
	rec1 := httptest.NewRecorder()
	ForwardOpencodeGo(rec1, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if got := atomic.LoadInt32(&k1Hits); got != 1 {
		t.Fatalf("k1 hits after first request = %d, want 1", got)
	}
	// Force k1's cooldown expiry while k2 is still cooling.
	providers[0].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())

	// Next request must probe k1 again (its cooldown is over, it is the
	// only non-cooling key), then fail over to cooling k2.
	rec2 := httptest.NewRecorder()
	ForwardOpencodeGo(rec2, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if got := atomic.LoadInt32(&k1Hits); got != 2 {
		t.Fatalf("k1 hits after cooldown expiry = %d, want 2 (recovered key re-probed)", got)
	}
}

// Sticky exhaustion end-to-end: k1 burns out, k2 takes over, and even after
// k1's cooldown expires it does NOT steal traffic back — k2 stays the lead
// until IT burns out. This is what keeps the upstream key session (and its
// prompt cache) alive.
func TestForwardOpencodeGoStickyKeepsLeadAfterRecovery(t *testing.T) {
	ResetRotationForTest()
	var k1Hits, k2Hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer k1":
			atomic.AddInt32(&k1Hits, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"Resets in 30 min"}}`))
		case "Bearer k2":
			atomic.AddInt32(&k2Hits, 1)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstreamSrv.Close()

	providers := []Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}

	// Request 1: k1 exhausted, k2 succeeds and becomes the lead.
	rec1 := httptest.NewRecorder()
	ForwardOpencodeGo(rec1, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", rec1.Code)
	}

	// k1's cooldown expires (e.g. 30-min window reset).
	providers[0].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())

	// Requests 2..4 must ALL stay on k2 — k1 is available but not the lead.
	for i := 2; i <= 4; i++ {
		rec := httptest.NewRecorder()
		ForwardOpencodeGo(rec, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&k1Hits); got != 1 {
		t.Fatalf("k1 hits = %d, want 1 (recovered key must not steal traffic)", got)
	}
	if got := atomic.LoadInt32(&k2Hits); got != 4 {
		t.Fatalf("k2 hits = %d, want 4 (sticky lead must burn until exhausted)", got)
	}
}


// The four forwarders (ForwardMinimax / ForwardOpencodeGo /
// ForwardOpencodeZen / ForwardOpenrouter) all rely on this contract:
// buildAttemptOrder NEVER filters by Family. Each forwarder is
// responsible for filtering the providers slice down to its own family
// before handing it to buildAttemptOrder. This test pins that
// contract: handing a slice with mixed families must produce a slice
// with the same mixed families back — only the order may change.
//
// A change to buildAttemptOrder that silently filters would not by
// itself leak the wrong key (no filtering at all is also valid), but
// this test locks in the "sort only" half of the contract that the
// per-forwarder tests below rely on.
func TestBuildAttemptOrderDoesNotFilterByFamily(t *testing.T) {
	ResetRotationForTest()
	ps := []*Provider{
		{Name: "minimax-1", Family: "minimax"},
		{Name: "zen-1", Family: "opencode-zen"},
		{Name: "openrouter-1", Family: "openrouter"},
		{Name: "go-1", Family: "opencode-go"},
		{Name: "minimax-2", Family: "minimax"},
	}
	got := buildAttemptOrder("minimax", ps)
	if len(got) != len(ps) {
		t.Fatalf("buildAttemptOrder dropped providers: got %d, want %d (filtering belongs in the forwarder, not here)", len(got), len(ps))
	}
	families := make(map[string]int)
	for _, p := range got {
		families[p.Family]++
	}
	wantFamilies := map[string]int{
		"minimax":     2,
		"opencode-zen": 1,
		"openrouter":   1,
		"opencode-go":  1,
	}
	for fam, n := range wantFamilies {
		if families[fam] != n {
			t.Errorf("family %q count = %d, want %d (buildAttemptOrder must not filter; the forwarder does)", fam, families[fam], n)
		}
	}
	for i, p := range ps {
		found := false
		for _, q := range got {
			if q.Name == p.Name && q.Family == p.Family {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("input provider[%d] %s (Family=%s) missing from output (input: %v, output: %v)", i, p.Name, p.Family, names(ps), names(got))
		}
	}
}

func names(ps []*Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
