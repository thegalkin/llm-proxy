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

func TestForwardMinimaxAllExhausted(t *testing.T) {
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
