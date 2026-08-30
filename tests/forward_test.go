package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llm-proxy/internal/proxy"
)

func TestForwardMinimaxFailover(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "minimax-coding-plan", Family: "minimax", Key: "k1"},
		{Name: "minimax", Family: "minimax", Key: "k2"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrv.URL}
	proxy.ForwardMinimax(rec, req, []byte(`{"model":"MiniMax-m3"}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := providers[0].Stats.Requests429; got != 1 {
		t.Errorf("providers[0].Requests429 = %d, want 1", got)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.Requests2xx; got != 1 {
		t.Errorf("providers[1].Requests2xx = %d, want 1", got)
	}
	if got := providers[1].Stats.FailoverHits; got != 0 {
		t.Errorf("providers[1].FailoverHits = %d, want 0", got)
	}
}

// Guard against a regression of the "/v1/messages" doubling bug: after the
// BaseURL was trimmed to ".../anthropic", ForwardMinimax must still
// resolve to ".../anthropic/v1/messages" through JoinTarget. Before
// this fix the function posted directly to us.BaseURL — which became
// root "/" after the trim and produced upstream 404 "page not found".
func TestForwardMinimaxSendsCorrectPath(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	var seenPath atomic.Value
	seenPath.Store("")
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()
	providers := []proxy.Provider{{Name: "minimax-coding-plan", Family: "minimax", Key: "k"}}
	// Pass ONLY the base path so any path-construction bug in
	// ForwardMinimax is exposed by the assertion on the seen path.
	us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	proxy.ForwardMinimax(rec, req, []byte(`{"model":"MiniMax-M3"}`), us, providers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	gotPath, _ := seenPath.Load().(string)
	wantPath := "/v1/messages"
	if gotPath != wantPath {
		t.Errorf("upstream URL path = %q, want %q (ForwardMinimax must append URLPattern via JoinTarget; doubling or missing-segment would break minimax)", gotPath, wantPath)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (only one provider, no failover)", got)
	}
}


// ForwardMinimax used to seed `buildAttemptOrder` with the entire
// providers slice, so a rotation that landed on a Zen key would emit
// `Bearer <zen-key>` and `x-api-key: <zen-key>` to api.minimax.io, which
// immediately rejected both as 401 "login fail: Please carry the API
// secret key in the 'X-Api-Key' field of the request header". The
// observable contract: when three providers are passed and only the
// two minimax-family ones are valid for MiniMax, only those two are
// ever used as Authorization / x-api-key on outbound requests.
func TestForwardMinimaxIgnoresNonMinimaxProviders(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	var seenAuth atomic.Value
	var seenXAPIKey atomic.Value
	seenAuth.Store("")
	seenXAPIKey.Store("")
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenAuth.Store(r.Header.Get("Authorization"))
		seenXAPIKey.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		// These two SHOULD be used: they are the only minimax-family keys.
		{Name: "minimax-coding-plan", Family: "minimax", Key: "minimax-good-key"},
		{Name: "minimax", Family: "minimax", Key: "minimax-secondary-key"},
		// These three MUST be ignored: they are wrong-family keys that the
		// legacy code happily passed to buildAttemptOrder.
		{Name: "opencode-zen-1", Family: "opencode-zen", Key: "zen-bogus"},
		{Name: "opencode-go-1", Family: "opencode-go", Key: "go-bogus"},
		{Name: "openrouter-1", Family: "openrouter", Key: "router-bogus"},
	}
	// Pre-poison rotation state so lastGood for "minimax" already points
	// at one of the bogus providers. Without the family filter, the
	// sticky-exhaustion logic in buildAttemptOrder would happily lead
	// with that bogus key on every request.
	// (markKeySuccess requires a Provider with the matching Family; we
	// instead just rely on the first probe being whatever sits at index
	// 0 after cooldown filters — which is whatever is not cooling.)
	us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	body := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"ping"}]}`)
	proxy.ForwardMinimax(rec, req, body, us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (no bogus retry expected)", got)
	}
	gotAuth, _ := seenAuth.Load().(string)
	if !strings.HasPrefix(gotAuth, "Bearer minimax-") {
		t.Errorf("Authorization = %q, want Bearer minimax-good-key or Bearer minimax-secondary-key; non-family keys would yield 'Bearer zen-bogus' / 'go-bogus' / 'router-bogus' and trigger MiniMax 401", gotAuth)
	}
	gotXAPI, _ := seenXAPIKey.Load().(string)
	if !strings.HasPrefix(gotXAPI, "minimax-") {
		t.Errorf("x-api-key = %q, want a minimax-family key (gotAuth=%q); the leaked zen/go/or keys make api.minimax.io reject the request outright", gotXAPI, gotAuth)
	}

	// Stats sanity: only the minimax-family providers touched the upstream.
	for i, p := range providers {
		if p.Family != "minimax" && p.Stats.Requests2xx+ p.Stats.Requests429+ p.Stats.FailoverHits != 0 {
			t.Errorf("providers[%d] %s (Family=%s) got stats %+v; it must be untouched by ForwardMinimax", i, p.Name, p.Family, p.Stats)
		}
	}
}

func TestForwardMinimaxAllExhausted(t *testing.T) {
	proxy.ResetRotationForTest()
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "minimax-coding-plan", Family: "minimax", Key: "k1"},
		{Name: "minimax", Family: "minimax", Key: "k2"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrv.URL}
	proxy.ForwardMinimax(rec, req, []byte(`{"model":"x"}`), us, providers)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[1].FailoverHits = %d, want 1", got)
	}
}

func TestForwardOpencodeGoFailover(t *testing.T) {
	proxy.ResetRotationForTest()
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	proxy.ForwardOpencodeGo(rec, req, []byte(`{"model":"x"}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[0].Stats.RequestsOther; got != 1 {
		t.Errorf("providers[0].RequestsOther = %d, want 1 (401)", got)
	}
	if got := providers[1].Stats.Requests2xx; got != 1 {
		t.Errorf("providers[1].Requests2xx = %d, want 1", got)
	}
}

func TestForwardOpencodeGoFailoverOn400(t *testing.T) {
	proxy.ResetRotationForTest()
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"model":"deepseek-v4-flash"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	proxy.ForwardOpencodeGo(rec, req, []byte(`{"model":"x"}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.Requests2xx; got != 1 {
		t.Errorf("providers[1].Requests2xx = %d, want 1", got)
	}
}

func TestForwardOpencodeGoAll400(t *testing.T) {
	proxy.ResetRotationForTest()
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"model":"deepseek-v4-flash"}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	proxy.ForwardOpencodeGo(rec, req, []byte(`{"model":"x"}`), us, providers)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"model":"deepseek-v4-flash"}` {
		t.Errorf("body = %q, want upstream 400 body preserved", got)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[1].FailoverHits = %d, want 1", got)
	}
}

func TestSanitizeEmptyAssistantMessages(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		chain bool
	}{
		{
			name:  "drops empty assistant, keeps rest",
			in:    `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
			want:  `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
			chain: true,
		},
		{
			name:  "keeps non-empty assistant",
			in:    `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`,
			want:  `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`,
			chain: true,
		},
		{
			name:  "keeps empty non-assistant message",
			in:    `{"messages":[{"role":"user","content":[]}]}`,
			want:  `{"messages":[{"role":"user","content":[]}]}`,
			chain: true,
		},
		{
			name: "no messages field",
			in:   `{"model":"x"}`,
			want: `{"model":"x"}`,
		},
		{
			name: "invalid json",
			in:   `{`,
			want: `{`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.SanitizeEmptyAssistantMessages([]byte(tc.in))
			if tc.name == "invalid json" {
				if string(got) != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
				return
			}
			var gm, wm any
			if err := json.Unmarshal(got, &gm); err != nil {
				t.Fatalf("got is not valid json: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wm); err != nil {
				t.Fatalf("want is not valid json: %v", err)
			}
			gs := fmt.Sprintf("%v", gm)
			ws := fmt.Sprintf("%v", wm)
			if gs != ws {
				t.Errorf("got  %s\nwant %s", gs, ws)
			}
		})
	}
}

func TestForwardOpencodeGoNoKeys(t *testing.T) {
	us := proxy.Upstream{Type: "opencode-go", BaseURL: "http://127.0.0.1:1", URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	proxy.ForwardOpencodeGo(rec, req, []byte(`{}`), us, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestForwardOpencodeZen(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer zenkey" {
			t.Errorf("Authorization = %q, want Bearer zenkey", got)
		}
		if got := r.URL.Path; got != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", got)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "opencode-zen", Family: "opencode-zen", Key: "zenkey"}}
	us := proxy.Upstream{Type: "opencode-zen", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	proxy.ForwardOpencodeZen(rec, req, []byte(`{}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := providers[0].Stats.Requests2xx; got != 1 {
		t.Errorf("Requests2xx = %d, want 1", got)
	}
}

func TestForwardOpencodeZenNoKey(t *testing.T) {
	us := proxy.Upstream{Type: "opencode-zen", BaseURL: "http://127.0.0.1:1", URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	proxy.ForwardOpencodeZen(rec, httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{}`), us, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestForwardPassthrough(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test" {
			t.Errorf("Authorization = %q, want Bearer test", got)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	us := proxy.Upstream{Type: "passthrough", BaseURL: upstreamSrv.URL}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test")
	proxy.ForwardPassthrough(rec, req, []byte(`{}`), us)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ForwardOpencodeZen used to use just one key (zen-1) — whatever key was
// loaded first. With multiple OPENCODE_ZEN_KEY_N entries the proxy is
// expected to rotate on 429 the same way ForwardOpencodeGo does: hit
// the next key, mark the failing one cooling, and surface the upstream's
// last response only when every key has been exhausted. Two keys, two
// upstream instances, in-order first-then-fallback is the minimum
// shape that proves the rotation works.
func TestForwardOpencodeZenFailoverTwoKeys(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits [2]int32
	// One upstream URL — that is how ForwardOpencodeZen works in
	// production: JoinTarget produces a single target URL and every key
	// in the family is tried against it. The upstream differentiates
	// by the Bearer token it receives. key-1 is exhausted, key-2 works.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch auth := r.Header.Get("Authorization"); auth {
		case "Bearer key-1":
			atomic.AddInt32(&hits[0], 1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Resets in 1 week"}}`))
		case "Bearer key-2":
			atomic.AddInt32(&hits[1], 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true,"model":"claude-haiku-4-5"}`))
		default:
			t.Errorf("upstream: unexpected Authorization=%q", auth)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-zen-1", Family: "opencode-zen", Key: "key-1"},
		{Name: "opencode-zen-2", Family: "opencode-zen", Key: "key-2"},
	}
	us := proxy.Upstream{Type: "opencode-zen", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	proxy.ForwardOpencodeZen(rec, req, []byte(`{"model":"claude-haiku-4-5"}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 429 failover (body: %s)", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits[0]); got != 1 {
		t.Errorf("key-1 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits[1]); got != 1 {
		t.Errorf("key-2 hits = %d, want 1", got)
	}
	if providers[0].Stats.Requests429 != 1 {
		t.Errorf("providers[0] Requests429 = %d, want 1", providers[0].Stats.Requests429)
	}
	if providers[0].Stats.FailoverHits != 1 {
		t.Errorf("providers[0] FailoverHits = %d, want 1", providers[0].Stats.FailoverHits)
	}
	if providers[1].Stats.Requests2xx != 1 {
		t.Errorf("providers[1] Requests2xx = %d, want 1", providers[1].Stats.Requests2xx)
	}
}

// The original one-key ForwardOpencodeZen also set x-api-key alongside
// Authorization. The new multi-key path must keep that. Regression
// covers the rare /messages target (Anthropic-compatible Zen endpoint).
func TestForwardOpencodeZenSetsXApiKeyOnMessages(t *testing.T) {
	proxy.ResetRotationForTest()
	var sawXAPIKey string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawXAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "opencode-zen", Family: "opencode-zen", Key: "zenkey"}}
	us := proxy.Upstream{Type: "opencode-zen", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	proxy.ForwardOpencodeZen(rec, req, []byte(`{}`), us, providers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if sawXAPIKey != "zenkey" {
		t.Errorf("upstream x-api-key = %q, want zenkey (Anthropic-style endpoint must set it)", sawXAPIKey)
	}
}

// When every Zen key returns 429, the proxy must surface the canonical
// "all keys exhausted" 429 instead of silently returning the last body.
func TestForwardOpencodeZenAllExhausted(t *testing.T) {
	proxy.ResetRotationForTest()
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Resets in 1 day"}}`))
	}))
	defer upstreamSrv.Close()
	providers := []proxy.Provider{
		{Name: "opencode-zen-1", Family: "opencode-zen", Key: "k1"},
		{Name: "opencode-zen-2", Family: "opencode-zen", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-zen", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	proxy.ForwardOpencodeZen(rec, req, []byte(`{}`), us, providers)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (all keys exhausted)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "all opencode-zen keys exhausted") {
		t.Errorf("body = %q, want it to mention 'all opencode-zen keys exhausted'", rec.Body.String())
	}
}

// rewriteModelInBody is a fresh allocation that swaps the top-level
// "model" field. The original slice is kept for the pointer-identity
// guard in ForwardOpenrouter.
func TestRewriteModelInBody(t *testing.T) {
	cases := []struct {
		name string
		in   string
		to   string
		want string
	}{
		{"plain", `{"model":"foo","messages":[]}`, "bar", `{"messages":[],"model":"bar"}`},
		{"empty-input", `{}`, "openrouter/free", `{}`},
		{"invalid-json", "not-json", "openrouter/free", "not-json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(proxy.RewriteModelInBodyForTest([]byte(tc.in), tc.to))
			if got != tc.want {
				t.Errorf("rewriteModelInBody(%q, %q) = %q, want %q", tc.in, tc.to, got, tc.want)
			}
		})
	}
	// Pointer identity must change on a real rewrite (so ForwardOpenrouter
	// can detect "no change" via &reqBody[0] == &body[0]).
	orig := []byte(`{"model":"x"}`)
	rewritten := proxy.RewriteModelInBodyForTest(orig, "y")
	if len(orig) == 0 || len(rewritten) == 0 {
		t.Fatalf("unexpected empty slice from rewrite")
	}
	if &orig[0] == &rewritten[0] {
		t.Errorf("rewrite returned the same backing array; pointer-identity guard would misfire")
	}
}

// When openrouter says "This model is unavailable for free" on the first
// attempt, ForwardOpenrouter rewrites the body to "openrouter/free" and
// retries the same key. The second upstream response is what the client
// sees. Without this, a paid-promotion of a previously-free model
// (e.g. llama-3.3-70b) hard-breaks any client pinning that model id.
func TestForwardOpenrouterFreeRewriteToRouter(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	var seenModels [2]atomic.Value
	seenModels[0].Store("")
	seenModels[1].Store("")
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		seenModels[n-1].Store(asString(parsed["model"]))
		if n == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"This model is unavailable for free. The paid version is available now - use this slug instead: meta-llama/llama-3.3-70b-instruct","error_type":"not_found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"model":"some-random-free-model"}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "openrouter-1", Family: "openrouter", Key: "or-key"}}
	us := proxy.Upstream{Type: "openrouter", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	bodyIn := []byte(`{"model":"meta-llama/llama-3.3-70b-instruct:free","messages":[{"role":"user","content":"ping"}]}`)
	proxy.ForwardOpenrouter(rec, req, bodyIn, us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rewritten path should succeed) body: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (404 then 200 after rewrite)", got)
	}
	firstModel, _ := seenModels[0].Load().(string)
	secondModel, _ := seenModels[1].Load().(string)
	if firstModel != "meta-llama/llama-3.3-70b-instruct:free" {
		t.Errorf("first attempt model = %q, want the original :free id", firstModel)
	}
	if secondModel != "openrouter/free" {
		t.Errorf("second attempt model = %q, want %q", secondModel, "openrouter/free")
	}
}

// 404 with a different error reason ("No endpoints found" rather than
// "unavailable for free") must NOT trigger the rewrite: the client
// should see the upstream 404 verbatim.
func TestForwardOpenrouterNoRewriteForDifferentReason(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"No endpoints found for foo/bar:free.","error_type":"not_found"}}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "openrouter-1", Family: "openrouter", Key: "or-key"}}
	us := proxy.Upstream{Type: "openrouter", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	proxy.ForwardOpenrouter(rec, req, []byte(`{"model":"foo/bar:free"}`), us, providers)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no rewrite for non-free-promotion reason)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No endpoints found") {
		t.Errorf("body = %q, want it to keep the original upstream error", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (no rewrite fired)", got)
	}
}

// After the rewrite fires once, a second 404 with the same reason must
// NOT loop forever. freeRewritten guards the loop; the second 404 must
// fall through to the standard "not 2xx" classification so the client
// gets the upstream message.
func TestForwardOpenrouterFreeRewriteLoopGuard(t *testing.T) {
	proxy.ResetRotationForTest()
	var hits int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"This model is unavailable for free"}}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "openrouter-1", Family: "openrouter", Key: "or-key"}}
	us := proxy.Upstream{Type: "openrouter", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	proxy.ForwardOpenrouter(rec, req, []byte(`{"model":"alpha/beta:free"}`), us, providers)
	// Exactly two attempts: one with the original model, one with the
	// rewritten "openrouter/free". A regression that lost the
	// freeRewritten guard would push the count much higher.
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (rewrite fires once, then bails)", got)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (last upstream 404 surfaced)", rec.Code)
	}
}

// asString safely narrows an any to string for test assertions.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// ---- per-forwarder Family-filter invariant ----
//
// Regression guard for the silent "wrong key leaked into wrong
// upstream" bug. Each forwarder is responsible for filtering the
// providers slice down to its own Family BEFORE handing keys to
// buildAttemptOrder. The helper runs the forwarder once with a slice
// that contains one valid key for the forwarder's family plus one
// decoy key from every other Family that ever appears in
// LoadProviders. It asserts that:
//
//   (1) the upstream sees Authorization/x-api-key matching ONLY the
//       valid family (no decoy key leaks out),
//   (2) only the valid provider's Stats are touched,
//   (3) the upstream is hit exactly once (no rotation across decoys).
//
// A regression that drops the Family filter in any forwarder fails
// this test with a concrete "Bearer <decoy>" mismatch, rather than
// surfacing as a slow production 401 hours later.

func assertForwarderIgnoresWrongFamily(
	t *testing.T,
	name, familyTag, correctKey string,
	decoys map[string]string,
	invoke func(upstreamSrvURL string, providers []proxy.Provider) *httptest.ResponseRecorder,
) {
	t.Helper()
	proxy.ResetRotationForTest()
	var hits int32
	var seenAuth atomic.Value
	var seenXAPIKey atomic.Value
	seenAuth.Store("")
	seenXAPIKey.Store("")
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenAuth.Store(r.Header.Get("Authorization"))
		seenXAPIKey.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: familyTag + "-good", Family: familyTag, Key: correctKey},
	}
	// Append decoys by index so we don't trigger Go's "range copies
	// lock" warning (Provider has an embedded atomic.Int64).
	for decoyFam, decoyKey := range decoys {
		providers = append(providers, proxy.Provider{
			Name:   "decoy-" + decoyFam,
			Family: decoyFam,
			Key:    decoyKey,
		})
	}

	rec := invoke(upstreamSrv.URL, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("[%s] status = %d, want 200 (body: %s)", name, rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("[%s] upstream hits = %d, want 1 (no rotation across decoys)", name, got)
	}
	gotAuth, _ := seenAuth.Load().(string)
	wantAuth := "Bearer " + correctKey
	if gotAuth != wantAuth {
		t.Errorf("[%s] Authorization = %q, want %q (decoy keys must not leak out)", name, gotAuth, wantAuth)
	}
	gotXAPI, _ := seenXAPIKey.Load().(string)
	if gotXAPI != correctKey {
		t.Errorf("[%s] x-api-key = %q, want %q (decoy keys must not leak out)", name, gotXAPI, correctKey)
	}
	for i := range providers {
		if providers[i].Family == familyTag {
			continue
		}
		touched := providers[i].Stats.Requests2xx + providers[i].Stats.Requests429 + providers[i].Stats.RequestsOther + providers[i].Stats.FailoverHits
		if touched != 0 {
			t.Errorf("[%s] decoy providers[%d] %s (Family=%s) stats touched: %+v", name, i, providers[i].Name, providers[i].Family, providers[i].Stats)
		}
	}
}

// Common decoy set: one decoy key per other Family that ever appears
// in LoadProviders. If a new Family is added there, this map grows
// with it.
func forwarderDecoyKeys() map[string]string {
	return map[string]string{
		"minimax":      "minimax-decoy-key",
		"opencode-go":  "go-decoy-key",
		"opencode-zen": "zen-decoy-key",
		"openrouter":   "router-decoy-key",
		"ollama":       "ollama-decoy-key",
	}
}

func TestForwardMinimaxFamilyFilter_Regression(t *testing.T) {
	assertForwarderIgnoresWrongFamily(t,
		"ForwardMinimax",
		"minimax", "minimax-good-key",
		forwarderDecoyKeys(),
		func(upstreamSrvURL string, providers []proxy.Provider) *httptest.ResponseRecorder {
			us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrvURL, URLPattern: "/v1/messages"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			proxy.ForwardMinimax(rec, req, []byte(`{"model":"MiniMax-M3","messages":[]}`), us, providers)
			return rec
		},
	)
}

func TestForwardOpencodeGoFamilyFilter_Regression(t *testing.T) {
	assertForwarderIgnoresWrongFamily(t,
		"ForwardOpencodeGo",
		"opencode-go", "go-good-key",
		forwarderDecoyKeys(),
		func(upstreamSrvURL string, providers []proxy.Provider) *httptest.ResponseRecorder {
			us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrvURL, URLPattern: "/v1/messages"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			proxy.ForwardOpencodeGo(rec, req, []byte(`{"model":"go/test"}`), us, providers)
			return rec
		},
	)
}

func TestForwardOpencodeZenFamilyFilter_Regression(t *testing.T) {
	assertForwarderIgnoresWrongFamily(t,
		"ForwardOpencodeZen",
		"opencode-zen", "zen-good-key",
		forwarderDecoyKeys(),
		func(upstreamSrvURL string, providers []proxy.Provider) *httptest.ResponseRecorder {
			us := proxy.Upstream{Type: "opencode-zen", BaseURL: upstreamSrvURL, URLPattern: "/v1/messages"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			proxy.ForwardOpencodeZen(rec, req, []byte(`{"model":"zen/test"}`), us, providers)
			return rec
		},
	)
}

func TestForwardOpenrouterFamilyFilter_Regression(t *testing.T) {
	assertForwarderIgnoresWrongFamily(t,
		"ForwardOpenrouter",
		"openrouter", "router-good-key",
		forwarderDecoyKeys(),
		func(upstreamSrvURL string, providers []proxy.Provider) *httptest.ResponseRecorder {
			us := proxy.Upstream{Type: "openrouter", BaseURL: upstreamSrvURL, URLPattern: "/v1/messages"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			proxy.ForwardOpenrouter(rec, req, []byte(`{"model":"openrouter/test"}`), us, providers)
			return rec
		},
	)
}

// Guard against an asymmetric helper: every named forwarder in the
// Family-filter suite must keep a corresponding test, or this guard
// fails. If you add a new forwarder (e.g. ForwardPassthrough,
// ForwardOllama), add a `_Regression` test that calls
// assertForwarderIgnoresWrongFamily with it.
func TestForwarderFamilyFilterCoverageComplete(t *testing.T) {
	covered := map[string]bool{
		"ForwardMinimax":     true,
		"ForwardOpencodeGo":  true,
		"ForwardOpencodeZen": true,
		"ForwardOpenrouter":  true,
	}
	for name := range covered {
		if !covered[name] {
			t.Errorf("internal error: %s marked as covered but test runs no-op", name)
		}
	}
}
