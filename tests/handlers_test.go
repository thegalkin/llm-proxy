package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"llm-proxy/internal/proxy"
)

func TestHandleHealthz(t *testing.T) {
	cfg := &proxy.Config{DefaultUS: proxy.Upstream{Type: "opencode-go"}, Rules: make([]proxy.Rule, 3)}
	rec := httptest.NewRecorder()
	proxy.HandleHealthz(cfg, []proxy.Provider{{Name: "a"}, {Name: "b"}})(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" || out["rules"] != float64(3) || out["providers"] != float64(2) {
		t.Errorf("unexpected healthz payload: %s", rec.Body.String())
	}
}

func TestHandleModels(t *testing.T) {
	cfg := &proxy.Config{
		DefaultUS: proxy.Upstream{Type: "opencode-go", Model: "deepseek-v4-flash"},
		Rules: []proxy.Rule{
			{Upstream: proxy.Upstream{Type: "opencode-go", Model: "GO/claude-sonnet-4-6"}},
			{Upstream: proxy.Upstream{Type: "opencode-go", Model: "kimi-k3"}},
			{Upstream: proxy.Upstream{Type: "opencode-go", Model: "GO/*"}}, // wildcard: skipped
		},
	}
	rec := httptest.NewRecorder()
	proxy.HandleModels(cfg)(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range out.Data {
		ids[d.ID] = true
	}
	for _, want := range []string{
		"llm-proxy/GO/GO/claude-sonnet-4-6",
		"llm-proxy/GO/kimi-k3",
		"llm-proxy/GO/deepseek-v4-flash",
		"llm-proxy/opencode-zen/zen/claude-sonnet-4-6",
	} {
		if !ids[want] {
			t.Errorf("model %q missing from /v1/models", want)
		}
	}
	if ids["llm-proxy/GO/GO/*"] {
		t.Error("wildcard model should be skipped")
	}
}

func TestHandleMessagesEndToEnd(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "k1" {
			t.Errorf("x-api-key = %q, want k1", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if body["model"] != "kimi-k3" {
			t.Errorf("upstream model = %v, want kimi-k3 (wildcard rule preserves capture)", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	cfg := &proxy.Config{
		DefaultUS: proxy.Upstream{Type: "opencode-go", BaseURL: proxy.OpencodeGoBaseURL, URLPattern: "/chat/completions"},
		Rules: []proxy.Rule{{
			MatchExpr:  "opencode-go/*",
			Upstream:   proxy.Upstream{Type: "opencode-go", Model: "*", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"},
			MatchRe:    compileFromPatternMust("opencode-go/*"),
			CaptureSrc: 1,
		}},
	}
	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}

	rec := httptest.NewRecorder()
	body := `{"model":"opencode-go/kimi-k3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	proxy.HandleMessages(cfg, providers)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(proxy.HeaderRoutingTag); got != "opencode-go:opencode-go/kimi-k3" {
		t.Errorf("routing tag = %q", got)
	}
}

func compileFromPatternMust(from string) (re *regexp.Regexp) {
	re, _, err := proxy.CompileFromPattern(from)
	if err != nil {
		panic(err)
	}
	return re
}
