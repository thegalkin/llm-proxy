package proxy_test

import (
	"encoding/json"
	"fmt"
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
