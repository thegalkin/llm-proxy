package proxy_test

import (
	"context"
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
			{Upstream: proxy.Upstream{Type: "minimax", Model: "MiniMax-m3"}},
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
		"llm-proxy/minimax/MiniMax-m3",
		"llm-proxy/GO/deepseek-v4-flash",
		"llm-proxy/GO/minimax-m3",
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
		if body["model"] != "minimax-m3" {
			t.Errorf("upstream model = %v, want minimax-m3 (wildcard rule preserves capture)", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	cfg := &proxy.Config{
		DefaultUS: proxy.Upstream{Type: "opencode-go", BaseURL: proxy.OpencodeGoBaseURL, URLPattern: "/chat/completions"},
		Rules: []proxy.Rule{{
			MatchExpr:  "minimax/minimax-*",
			Upstream:   proxy.Upstream{Type: "minimax", Model: "minimax-*", BaseURL: upstreamSrv.URL + "/v1/messages"},
			MatchRe:    compileFromPatternMust("minimax/minimax-*"),
			CaptureSrc: 1,
			PreserveTo: "minimax-",
		}},
	}
	providers := []proxy.Provider{{Name: "minimax-coding-plan", Family: "minimax", Key: "k1"}}

	rec := httptest.NewRecorder()
	body := `{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	proxy.HandleMessages(cfg, providers)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(proxy.HeaderRoutingTag); got != "minimax:minimax/minimax-m3" {
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

func TestHandleLimitsOpencodeGo(t *testing.T) {
	cfg := &proxy.Config{DefaultUS: proxy.Upstream{Type: "opencode-go"}}
	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k"}}
	rec := httptest.NewRecorder()
	proxy.HandleLimits(cfg, providers)(rec, httptest.NewRequest(http.MethodGet, "/admin/limits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Providers []proxy.ProviderReport `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(out.Providers))
	}
	if out.Providers[0].Quota.OK {
		t.Error("opencode-go quota should not be OK")
	}
	if out.Providers[0].Quota.Error == "" {
		t.Error("expected a quota error message")
	}
}

func TestParseQuotaPayload(t *testing.T) {
	payload := `{
		"model_remains": [
			{
				"model_name": "general",
				"current_interval_remaining_percent": 42.5,
				"current_interval_total_count": 100,
				"current_interval_usage_count": 58,
				"current_interval_status": 1,
				"start_time": 1700000000000,
				"end_time": 1700036000000,
				"current_weekly_remaining_percent": 12.0,
				"current_weekly_total_count": 1000,
				"current_weekly_usage_count": 880,
				"current_weekly_status": 0,
				"weekly_start_time": 1699400000000,
				"weekly_end_time": 1700000000000
			}
		]
	}`
	// nowUnix such that end_time - now = 3600s
	const nowUnix = 1700032400
	quota, err := proxy.ParseQuotaPayload([]byte(payload), nowUnix)
	if err != nil {
		t.Fatalf("parseQuotaPayload: %v", err)
	}
	fiveH := quota.FiveH
	if fiveH.PctRemaining == nil || *fiveH.PctRemaining != 42 {
		t.Errorf("PctRemaining = %v, want 42", fiveH.PctRemaining)
	}
	if fiveH.TotalCount == nil || *fiveH.TotalCount != 100 {
		t.Errorf("TotalCount = %v, want 100", fiveH.TotalCount)
	}
	if fiveH.ConsumedCount == nil || *fiveH.ConsumedCount != 42 {
		t.Errorf("ConsumedCount = %v, want 42", fiveH.ConsumedCount)
	}
	if fiveH.ResetInS == nil || *fiveH.ResetInS != 3600 {
		t.Errorf("ResetInS = %v, want 3600", fiveH.ResetInS)
	}
	if fiveH.WindowEndUnix == nil || *fiveH.WindowEndUnix != 1700036000 {
		t.Errorf("WindowEndUnix = %v, want 1700036000", fiveH.WindowEndUnix)
	}
	weekly := quota.Weekly
	if weekly.TotalCount == nil || *weekly.TotalCount != 1000 {
		t.Errorf("weekly TotalCount = %v, want 1000", weekly.TotalCount)
	}
	if weekly.RemainingCount == nil || *weekly.RemainingCount != 880 {
		t.Errorf("weekly RemainingCount = %v, want 880", weekly.RemainingCount)
	}
	if weekly.ConsumedCount == nil || *weekly.ConsumedCount != 120 {
		t.Errorf("weekly ConsumedCount = %v, want 120", weekly.ConsumedCount)
	}
}

func TestParseQuotaPayloadBad(t *testing.T) {
	if _, err := proxy.ParseQuotaPayload([]byte(`not json`), 0); err == nil {
		t.Error("invalid JSON should fail")
	}
	if _, err := proxy.ParseQuotaPayload([]byte(`{"model_remains":[]}`), 0); err == nil {
		t.Error("missing general entry should fail")
	}
}

func TestProbeProviderQuotaOpencodeGo(t *testing.T) {
	report := proxy.ProbeProviderQuota(context.Background(), proxy.Provider{Name: "go-1", Family: "opencode-go", Key: "k"})
	if report.Quota.OK {
		t.Error("OK should be false")
	}
	if report.Quota.Error == "" {
		t.Error("expected error message")
	}
}
